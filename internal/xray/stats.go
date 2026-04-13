package xray

import (
	"context"
	"log"
	"sync"
	"time"

	"xray-panel/internal/models"
)

type StatsCollector struct {
	client   *Client
	profiles *models.VPNProfileStore

	// pending хранит трафик, который не удалось записать в БД
	pending map[string][2]int64

	// Кумулятивный трафик в памяти — для быстрого enforce без запроса к БД.
	// Ключ — UUID, значение — total bytes (из БД + все дельты).
	cumulative   map[string]int64
	cumulativeMu sync.Mutex

	// Лимиты в памяти — чтобы не дёргать БД на каждую проверку
	limits   map[string]int64 // uuid → traffic_limit (0 = без лимита)
	limitsMu sync.RWMutex

	// Кэш для HTTP-хендлеров
	snapMu        sync.RWMutex
	lastTraffic   map[string][2]int64
	lastOnline    map[string]bool
	lastOnlineIPs map[string][]string

	lastFullEnforce time.Time
}

func NewStatsCollector(client *Client, profiles *models.VPNProfileStore) *StatsCollector {
	return &StatsCollector{
		client:     client,
		profiles:   profiles,
		pending:    make(map[string][2]int64),
		cumulative: make(map[string]int64),
		limits:     make(map[string]int64),
	}
}

// Snapshot возвращает потокобезопасный снимок для HTTP-хендлеров.
func (s *StatsCollector) Snapshot() (
	traffic map[string][2]int64,
	online map[string]bool,
	ips map[string][]string,
) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	return s.lastTraffic, s.lastOnline, s.lastOnlineIPs
}

// InitCumulative загружает текущий трафик и лимиты из БД в память.
// Вызывать при старте и периодически для ресинхронизации.
func (s *StatsCollector) InitCumulative(ctx context.Context) {
	profiles, err := s.profiles.ListAll(ctx)
	if err != nil {
		log.Printf("Stats: failed to init cumulative: %v", err)
		return
	}

	s.cumulativeMu.Lock()
	s.limitsMu.Lock()

	for _, p := range profiles {
		s.cumulative[p.UUID] = p.TrafficUp + p.TrafficDown
		s.limits[p.UUID] = p.TrafficLimit
	}

	s.limitsMu.Unlock()
	s.cumulativeMu.Unlock()

	log.Printf("Stats: cumulative initialized for %d profiles", len(profiles))
}

// UpdateLimit обновляет лимит в кэше (вызывать из админки при смене лимита)
func (s *StatsCollector) UpdateLimit(uuid string, limitBytes int64) {
	s.limitsMu.Lock()
	s.limits[uuid] = limitBytes
	s.limitsMu.Unlock()
}

// ResetCumulative сбрасывает кумулятивный счётчик (вызывать при сбросе трафика)
func (s *StatsCollector) ResetCumulative(uuid string) {
	s.cumulativeMu.Lock()
	s.cumulative[uuid] = 0
	s.cumulativeMu.Unlock()
}

func (s *StatsCollector) Run(ctx context.Context) {
	log.Println("Stats collector started")

	// Быстрый цикл: сбор трафика + enforce (каждую секунду)
	fastTicker := time.NewTicker(1 * time.Second)
	defer fastTicker.Stop()

	// Медленный цикл: онлайн-статус и IP (каждые 5 секунд)
	slowTicker := time.NewTicker(5 * time.Second)
	defer slowTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stats collector stopped")
			return
		case <-fastTicker.C:
			s.collectAndEnforce(ctx)
		case <-slowTicker.C:
			s.updateOnlineStatus(ctx)
		}
	}
}

// collectAndEnforce — быстрый цикл: один gRPC → in-memory enforce → запись в БД
func (s *StatsCollector) collectAndEnforce(ctx context.Context) {
	// Один вызов со сбросом — получаем дельту и обнуляем счётчики Xray
	traffic, err := s.client.QueryAllUserTraffic(ctx, true)
	if err != nil {
		log.Printf("Stats collect error: %v", err)
		return
	}

	// Объединяем с pending (незаписанные ранее дельты)
	for email, stats := range s.pending {
		entry := traffic[email]
		entry[0] += stats[0]
		entry[1] += stats[1]
		traffic[email] = entry
	}
	clear(s.pending)

	// Сохраняем дельту в snapshot для HTTP-хендлеров
	s.snapMu.Lock()
	s.lastTraffic = traffic
	s.snapMu.Unlock()

	updated := 0
	for uuid, stats := range traffic {
		up, down := stats[0], stats[1]
		if up == 0 && down == 0 {
			continue
		}

		delta := up + down

		// 1. Обновляем кумулятивный счётчик в памяти
		s.cumulativeMu.Lock()
		s.cumulative[uuid] += delta
		currentTotal := s.cumulative[uuid]
		s.cumulativeMu.Unlock()

		// 2. Проверяем лимит по памяти — без единого запроса к БД
		s.limitsMu.RLock()
		limit := s.limits[uuid]
		s.limitsMu.RUnlock()

		if limit > 0 && currentTotal >= limit {
			s.disconnectUser(ctx, uuid, currentTotal, limit)
		}

		// 3. Пишем в БД
		if err := s.profiles.UpdateTraffic(ctx, uuid, up, down); err != nil {
			log.Printf("Stats update error for %s: %v — will retry", uuid, err)
			s.pending[uuid] = [2]int64{up, down}
			continue
		}
		updated++
	}

	if updated > 0 {
		log.Printf("Stats collected: %d users updated", updated)
	}

	// Подстраховка: полная проверка раз в минуту
	if time.Since(s.lastFullEnforce) > time.Minute {
		s.enforceAll(ctx)
		s.lastFullEnforce = time.Now()
	}
}

// disconnectUser отключает пользователя: убивает TCP-сессии, удаляет из Xray, деактивирует в БД
func (s *StatsCollector) disconnectUser(ctx context.Context, uuid string, total, limit int64) {
	// 1. Сначала убиваем активные TCP-соединения (пока юзер ещё в Xray и IP доступны)
	killed := s.client.KillConnections(ctx, uuid)

	// 2. Удаляем из Xray (новые соединения больше не пройдут)
	if err := s.client.RemoveUser(ctx, uuid); err != nil {
		log.Printf("Enforce: failed to remove %s: %v", uuid, err)
		return
	}

	// 3. Деактивируем в БД
	p, err := s.profiles.GetByUUID(ctx, uuid)
	if err != nil {
		log.Printf("Enforce: profile %s not found: %v", uuid, err)
		return
	}

	if err := s.profiles.SetActive(ctx, p.ID, false); err != nil {
		log.Printf("Enforce: failed to deactivate %s: %v", uuid, err)
		return
	}

	overshoot := total - limit
	log.Printf("Enforce: %s disabled — limit %d, total %d, overshoot %.1f MB, killed %d connections",
		uuid, limit, total, float64(overshoot)/(1024*1024), killed)
}

// updateOnlineStatus — медленный цикл: онлайн-статус и IP-адреса
func (s *StatsCollector) updateOnlineStatus(ctx context.Context) {
	onlineUsers := make(map[string]bool)

	s.snapMu.RLock()
	liveTraffic := s.lastTraffic
	s.snapMu.RUnlock()

	if online, err := s.client.GetOnlineUsers(ctx, liveTraffic); err == nil {
		onlineUsers = online
	}

	onlineIPs := s.client.GetOnlineIPs(ctx, onlineUsers)

	s.snapMu.Lock()
	s.lastOnline = onlineUsers
	s.lastOnlineIPs = onlineIPs
	s.snapMu.Unlock()
}

// enforceAll — полная проверка всех профилей (подстраховка для expires_at и рассинхрона)
func (s *StatsCollector) enforceAll(ctx context.Context) {
	exceeded, err := s.profiles.GetExceeded(ctx)
	if err != nil {
		log.Printf("EnforceAll: failed to get exceeded profiles: %v", err)
		return
	}

	for _, p := range exceeded {
		if err := s.client.RemoveUser(ctx, p.UUID); err != nil {
			log.Printf("EnforceAll: failed to remove %s: %v", p.UUID, err)
			continue
		}
		if err := s.profiles.SetActive(ctx, p.ID, false); err != nil {
			log.Printf("EnforceAll: failed to deactivate %s: %v", p.UUID, err)
			continue
		}

		reason := "traffic limit"
		if p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now()) {
			reason = "expired"
		}
		log.Printf("EnforceAll: %s disabled — %s", p.UUID, reason)
	}

	if len(exceeded) > 0 {
		log.Printf("EnforceAll: %d profiles disabled", len(exceeded))
	}

	// Ресинхронизируем cumulative и limits из БД
	s.InitCumulative(ctx)
}
