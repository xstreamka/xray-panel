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
}

func NewStatsCollector(client *Client, profiles *models.VPNProfileStore, interval time.Duration) *StatsCollector {
	return &StatsCollector{
		client:   client,
		profiles: profiles,
		interval: interval,
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
	// Это исключает потерю трафика между отдельными шагами query и reset.
	traffic, err := s.client.QueryAllUserTraffic(ctx, true)
	if err != nil {
		log.Printf("Stats collect error: %v", err)
		return
	}

	updated := 0
	for email, stats := range traffic {
		up, down := stats[0], stats[1]
		if up == 0 && down == 0 {
			continue
		}

		if err := s.profiles.UpdateTraffic(ctx, email, up, down); err != nil {
			log.Printf("Stats update error for %s: %v", email, err)
		} else {
			updated++
		}
	}

	if updated > 0 {
		log.Printf("Stats collected: %d users updated", updated)
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
