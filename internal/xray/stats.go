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
	users    *models.UserStore
	firewall *Firewall

	// pending хранит трафик, который не удалось записать в БД
	pending map[string][2]int64

	// Кумулятивный трафик в памяти — для быстрого enforce без запроса к БД.
	// Ключ — UUID, значение — total bytes (из БД + все дельты).
	cumulative   map[string]int64
	cumulativeMu sync.Mutex

	// Лимиты в памяти — чтобы не дёргать БД на каждую проверку.
	// Это personal-лимит на профиль (пользовательская фича),
	// НЕ общий лимит подписки — тот считается на уровне user.
	limits   map[string]int64 // uuid → traffic_limit (0 = без лимита)
	limitsMu sync.RWMutex

	// Мэппинг UUID профиля → user_id владельца.
	// Нужен для агрегации дельт трафика по юзеру и вызова DeductTraffic.
	uuidToUser   map[string]int
	uuidToUserMu sync.RWMutex

	// disabledUsers — юзеры, чей баланс уже исчерпан в этом цикле enforcement.
	// Чтобы не спамить DeductTraffic и DisconnectUserAll в каждом тике
	// до ближайшей ресинхронизации.
	disabledUsers   map[int]bool
	disabledUsersMu sync.Mutex

	// Кэш для HTTP-хендлеров
	snapMu        sync.RWMutex
	lastTraffic   map[string][2]int64
	lastOnline    map[string]bool
	lastOnlineIPs map[string][]string

	lastFullEnforce time.Time
}

func NewStatsCollector(
	client *Client,
	profiles *models.VPNProfileStore,
	users *models.UserStore,
	firewall *Firewall,
) *StatsCollector {
	return &StatsCollector{
		client:        client,
		profiles:      profiles,
		users:         users,
		firewall:      firewall,
		pending:       make(map[string][2]int64),
		cumulative:    make(map[string]int64),
		limits:        make(map[string]int64),
		uuidToUser:    make(map[string]int),
		disabledUsers: make(map[int]bool),
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
// Также сбрасывает disabledUsers: следующий цикл перепроверит баланс юзера,
// и если он был пополнен — spam DeductTraffic возобновится штатно.
func (s *StatsCollector) InitCumulative(ctx context.Context) {
	profiles, err := s.profiles.ListAll(ctx)
	if err != nil {
		log.Printf("Stats: failed to init cumulative: %v", err)
		return
	}

	s.cumulativeMu.Lock()
	s.limitsMu.Lock()
	s.uuidToUserMu.Lock()

	clear(s.uuidToUser)
	for _, p := range profiles {
		s.cumulative[p.UUID] = p.TrafficUp + p.TrafficDown
		s.limits[p.UUID] = p.TrafficLimit
		s.uuidToUser[p.UUID] = p.UserID
	}

	s.uuidToUserMu.Unlock()
	s.limitsMu.Unlock()
	s.cumulativeMu.Unlock()

	s.disabledUsersMu.Lock()
	clear(s.disabledUsers)
	s.disabledUsersMu.Unlock()

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

	// Агрегация дельт по user_id для списания с подписочного баланса.
	byUser := make(map[int]int64)

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

		// 2. Проверяем personal-лимит профиля (пользовательская фича)
		s.limitsMu.RLock()
		limit := s.limits[uuid]
		s.limitsMu.RUnlock()

		if limit > 0 && currentTotal >= limit {
			s.disconnectUser(ctx, uuid, currentTotal, limit)
		}

		// 3. Копим дельту в байтах для списания с баланса юзера
		s.uuidToUserMu.RLock()
		if uid, ok := s.uuidToUser[uuid]; ok {
			byUser[uid] += delta
		}
		s.uuidToUserMu.RUnlock()

		// 4. Пишем per-profile статистику в БД (для отображения)
		if err := s.profiles.UpdateTraffic(ctx, uuid, up, down); err != nil {
			log.Printf("Stats update error for %s: %v — will retry", uuid, err)
			s.pending[uuid] = [2]int64{up, down}
			continue
		}
		updated++
	}

	// Списываем суммарные дельты с подписочного баланса юзеров.
	// Если баланс исчерпан — отключаем все профили юзера разом.
	for uid, sum := range byUser {
		if sum <= 0 {
			continue
		}
		s.disabledUsersMu.Lock()
		skip := s.disabledUsers[uid]
		s.disabledUsersMu.Unlock()
		if skip {
			continue
		}

		remaining, err := s.users.DeductTraffic(ctx, uid, sum)
		if err != nil {
			log.Printf("Stats: DeductTraffic user=%d delta=%d: %v", uid, sum, err)
			continue
		}
		if remaining <= 0 {
			s.DisconnectUserAll(ctx, uid, "balance exhausted")
			s.disabledUsersMu.Lock()
			s.disabledUsers[uid] = true
			s.disabledUsersMu.Unlock()
		}
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

// DisconnectUserAll отключает все профили юзера по user_id:
// блокирует TCP всех его UUID, удаляет из Xray, снимает is_active в БД.
// Используется, когда баланс подписки исчерпан или истёк срок подписки.
// Экспортирован, чтобы подписочный воркер мог вызывать на истёкших юзерах.
func (s *StatsCollector) DisconnectUserAll(ctx context.Context, userID int, reason string) {
	profiles, err := s.profiles.GetByUserID(ctx, userID)
	if err != nil {
		log.Printf("Enforce: GetByUserID %d: %v", userID, err)
		return
	}

	blocked := 0
	for _, p := range profiles {
		if !p.IsActive {
			continue
		}
		blocked += s.firewall.BlockUser(ctx, s.client, p.UUID)
		if err := s.client.RemoveUser(ctx, p.UUID); err != nil {
			log.Printf("Enforce: RemoveUser %s: %v (may be already removed)", p.UUID, err)
		}
		// Обнулим personal-лимит в кэше, чтобы per-profile enforcement не гонялся
		// повторно — записи оживут при ресинхронизации InitCumulative.
		s.limitsMu.Lock()
		s.limits[p.UUID] = 0
		s.limitsMu.Unlock()
	}

	uuids, err := s.profiles.DeactivateAllByUser(ctx, userID)
	if err != nil {
		log.Printf("Enforce: DeactivateAllByUser %d: %v", userID, err)
		return
	}

	log.Printf("Enforce: user=%d disabled (%s) — %d profiles, %d IPs blocked",
		userID, reason, len(uuids), blocked)
}

// disconnectUser отключает ОДИН профиль по personal-лимиту (пользовательская фича).
// Это не связано с балансом подписки — только с лимитом конкретного профиля.
func (s *StatsCollector) disconnectUser(ctx context.Context, uuid string, total, limit int64) {
	// Сразу обнуляем лимит в кэше, чтобы следующие циклы не спамили повторными попытками
	s.limitsMu.Lock()
	s.limits[uuid] = 0
	s.limitsMu.Unlock()

	// 1. Блокируем TCP через iptables REJECT (правила живут до реактивации!)
	blocked := s.firewall.BlockUser(ctx, s.client, uuid)

	// 2. Удаляем из Xray (ошибка "not found" — не страшно, мог быть удалён ранее)
	if err := s.client.RemoveUser(ctx, uuid); err != nil {
		log.Printf("Enforce: RemoveUser %s: %v (may be already removed)", uuid, err)
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
	log.Printf("Enforce: %s disabled — limit %d, total %d, overshoot %.1f MB, blocked %d IPs",
		uuid, limit, total, float64(overshoot)/(1024*1024), blocked)
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
		s.firewall.BlockUser(ctx, s.client, p.UUID)
		if err := s.client.RemoveUser(ctx, p.UUID); err != nil {
			log.Printf("EnforceAll: RemoveUser %s: %v", p.UUID, err)
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
