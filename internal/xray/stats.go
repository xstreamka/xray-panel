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
	interval time.Duration

	// pending хранит трафик, который не удалось записать в БД
	pending map[string][2]int64

	// Кэш для HTTP-хендлеров (чтобы не дёргать gRPC на каждый запрос)
	snapMu        sync.RWMutex
	lastTraffic   map[string][2]int64
	lastOnline    map[string]bool
	lastOnlineIPs map[string][]string

	// Подстраховка: полная проверка лимитов раз в минуту
	lastFullEnforce time.Time
}

func NewStatsCollector(client *Client, profiles *models.VPNProfileStore, interval time.Duration) *StatsCollector {
	return &StatsCollector{
		client:   client,
		profiles: profiles,
		interval: interval,
		pending:  make(map[string][2]int64),
	}
}

// Snapshot возвращает потокобезопасный снимок статистики для HTTP-хендлеров.
// Трафик — это незаписанные дельты с момента последнего reset (для отображения «живых» цифр).
// Хендлеры должны прибавлять эти дельты к значениям из БД.
func (s *StatsCollector) Snapshot() (
	traffic map[string][2]int64,
	online map[string]bool,
	ips map[string][]string,
) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	return s.lastTraffic, s.lastOnline, s.lastOnlineIPs
}

func (s *StatsCollector) Run(ctx context.Context) {
	log.Printf("Stats collector started (interval: %s)", s.interval)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stats collector stopped")
			return
		case <-ticker.C:
			s.collect(ctx)
		}
	}
}

func (s *StatsCollector) collect(ctx context.Context) {
	// 1. Получаем живой трафик БЕЗ сброса — для снимка и онлайн-статуса
	liveTraffic, err := s.client.QueryAllUserTraffic(ctx, false)
	if err != nil {
		log.Printf("Stats collect error (live): %v", err)
		return
	}

	// 2. Обновляем онлайн-статус
	onlineUsers := make(map[string]bool)
	if online, err := s.client.GetOnlineUsers(ctx, liveTraffic); err == nil {
		onlineUsers = online
	}
	onlineIPs := s.client.GetOnlineIPs(ctx, onlineUsers)

	// 3. Сохраняем снимок для HTTP-хендлеров
	s.snapMu.Lock()
	s.lastTraffic = liveTraffic
	s.lastOnline = onlineUsers
	s.lastOnlineIPs = onlineIPs
	s.snapMu.Unlock()

	// 4. Теперь забираем дельту СО СБРОСОМ — для записи в БД
	traffic, err := s.client.QueryAllUserTraffic(ctx, true)
	if err != nil {
		log.Printf("Stats collect error (reset): %v", err)
		return
	}

	// Объединяем с pending
	for email, stats := range s.pending {
		entry := traffic[email]
		entry[0] += stats[0]
		entry[1] += stats[1]
		traffic[email] = entry
	}
	clear(s.pending)

	updated := 0
	for email, stats := range traffic {
		up, down := stats[0], stats[1]
		if up == 0 && down == 0 {
			continue
		}

		if err := s.profiles.UpdateTraffic(ctx, email, up, down); err != nil {
			log.Printf("Stats update error for %s: %v — will retry", email, err)
			s.pending[email] = [2]int64{up, down}
			continue
		}
		updated++

		// Сразу проверяем лимит после записи
		s.checkAndEnforce(ctx, email)
	}

	if updated > 0 {
		log.Printf("Stats collected: %d users updated", updated)
	}
	if len(s.pending) > 0 {
		log.Printf("Stats pending: %d users will be retried next cycle", len(s.pending))
	}

	// Подстраховка: полная проверка раз в минуту (на случай expires_at)
	if time.Since(s.lastFullEnforce) > time.Minute {
		s.enforceAll(ctx)
		s.lastFullEnforce = time.Now()
	}
}

// checkAndEnforce — быстрая проверка одного профиля сразу после записи трафика
func (s *StatsCollector) checkAndEnforce(ctx context.Context, uuid string) {
	p, err := s.profiles.GetByUUID(ctx, uuid)
	if err != nil {
		return
	}

	if !p.IsActive {
		return
	}

	total := p.TrafficUp + p.TrafficDown
	expired := p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now())
	overLimit := p.TrafficLimit > 0 && total >= p.TrafficLimit

	if !expired && !overLimit {
		return
	}

	if err := s.client.RemoveUser(ctx, p.UUID); err != nil {
		log.Printf("Enforce: failed to remove %s from Xray: %v", p.UUID, err)
		return
	}

	if err := s.profiles.SetActive(ctx, p.ID, false); err != nil {
		log.Printf("Enforce: failed to deactivate %s in DB: %v", p.UUID, err)
		return
	}

	reason := "traffic limit"
	if expired {
		reason = "expired"
	}
	log.Printf("Enforce: %s (user_id=%d) disabled — %s (total: %d, limit: %d)",
		p.UUID, p.UserID, reason, total, p.TrafficLimit)
}

// enforceAll — полная проверка всех профилей (подстраховка, раз в минуту)
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
}
