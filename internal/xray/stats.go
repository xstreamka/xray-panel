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

// Run запускает фоновый сбор статистики. Блокирующий — запускать в горутине.
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
	// Получаем стату по всем юзерам и сбрасываем счётчики в Xray
	traffic, err := s.client.QueryAllUserTraffic(ctx, true)
	if err != nil {
		log.Printf("Stats collect error: %v", err)
		return
	}

	for email, stats := range traffic {
		up, down := stats[0], stats[1]
		if up == 0 && down == 0 {
			continue
		}

		// email = uuid (мы используем UUID как email в Xray)
		if err := s.profiles.UpdateTraffic(ctx, email, up, down); err != nil {
			log.Printf("Stats update error for %s: %v", email, err)
		}
	}

	if len(traffic) > 0 {
		log.Printf("Stats collected: %d users updated", len(traffic))
	}
}
