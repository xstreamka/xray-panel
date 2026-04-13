package xray

import (
	"context"
	"log"
	"time"

	"xray-panel/internal/models"
)

type StatsCollector struct {
	client   *Client
	profiles *models.VPNProfileStore
	interval time.Duration
	// pending хранит трафик, который не удалось записать в БД.
	// Эти дельты добавляются к следующему циклу сбора, чтобы не терять трафик.
	pending map[string][2]int64
}

func NewStatsCollector(client *Client, profiles *models.VPNProfileStore, interval time.Duration) *StatsCollector {
	return &StatsCollector{
		client:   client,
		profiles: profiles,
		interval: interval,
		pending:  make(map[string][2]int64),
	}
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
			s.enforceLimit(ctx)
		}
	}
}

func (s *StatsCollector) collect(ctx context.Context) {
	// Атомарный запрос со сбросом — получаем дельту и сразу обнуляем счётчики Xray.
	// Это исключает двойной подсчёт трафика (старая проблема "200 вместо 100").
	traffic, err := s.client.QueryAllUserTraffic(ctx, true)
	if err != nil {
		log.Printf("Stats collect error: %v", err)
		return
	}

	// Объединяем новые данные с pending (незаписанными ранее дельтами)
	for email, stats := range s.pending {
		entry := traffic[email]
		entry[0] += stats[0]
		entry[1] += stats[1]
		traffic[email] = entry
	}
	// Очищаем pending — при ошибке запишем обратно
	clear(s.pending)

	updated := 0
	for email, stats := range traffic {
		up, down := stats[0], stats[1]
		if up == 0 && down == 0 {
			continue
		}

		if err := s.profiles.UpdateTraffic(ctx, email, up, down); err != nil {
			log.Printf("Stats update error for %s (up=%d, down=%d): %v — will retry", email, up, down, err)
			// Сохраняем в pending для повторной попытки в следующем цикле
			s.pending[email] = [2]int64{up, down}
		} else {
			updated++
		}
	}

	if updated > 0 {
		log.Printf("Stats collected: %d users updated", updated)
	}
	if len(s.pending) > 0 {
		log.Printf("Stats pending: %d users will be retried next cycle", len(s.pending))
	}
}

// enforceLimit проверяет лимиты и отключает превысивших
func (s *StatsCollector) enforceLimit(ctx context.Context) {
	// 1. Получаем профили, которые превысили лимит или истекли
	exceeded, err := s.profiles.GetExceeded(ctx)
	if err != nil {
		log.Printf("Enforce: failed to get exceeded profiles: %v", err)
		return
	}

	for _, p := range exceeded {
		// Удаляем из Xray
		if err := s.client.RemoveUser(ctx, p.UUID); err != nil {
			log.Printf("Enforce: failed to remove %s from Xray: %v", p.UUID, err)
			continue
		}

		// Деактивируем в БД
		if err := s.profiles.SetActive(ctx, p.ID, false); err != nil {
			log.Printf("Enforce: failed to deactivate %s in DB: %v", p.UUID, err)
			continue
		}

		reason := "traffic limit"
		if p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now()) {
			reason = "expired"
		}

		log.Printf("Enforce: profile %s (user_id=%d) disabled — %s", p.UUID, p.UserID, reason)
	}

	if len(exceeded) > 0 {
		log.Printf("Enforce: %d profiles disabled", len(exceeded))
	}
}
