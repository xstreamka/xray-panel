package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type MTProtoTrafficLogStore struct {
	pool *pgxpool.Pool
}

func NewMTProtoTrafficLogStore(pool *pgxpool.Pool) *MTProtoTrafficLogStore {
	return &MTProtoTrafficLogStore{pool: pool}
}

func (s *MTProtoTrafficLogStore) UpsertBatch(ctx context.Context, bucket time.Time, entries map[int][2]int64) error {
	if len(entries) == 0 {
		return nil
	}

	var (
		placeholders = make([]string, 0, len(entries))
		args         = make([]any, 0, len(entries)*4)
		i            = 1
	)
	for profileID, stats := range entries {
		if stats[0] == 0 && stats[1] == 0 {
			continue
		}
		placeholders = append(placeholders, fmt.Sprintf("($%d,$%d,$%d,$%d)", i, i+1, i+2, i+3))
		args = append(args, profileID, bucket, stats[0], stats[1])
		i += 4
	}
	if len(placeholders) == 0 {
		return nil
	}

	q := `INSERT INTO mtproto_traffic_logs (profile_id, logged_at, bytes_up, bytes_down) VALUES ` +
		strings.Join(placeholders, ",") +
		` ON CONFLICT (profile_id, logged_at) DO UPDATE SET
		   bytes_up   = mtproto_traffic_logs.bytes_up   + EXCLUDED.bytes_up,
		   bytes_down = mtproto_traffic_logs.bytes_down + EXCLUDED.bytes_down`

	if _, err := s.pool.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("mtproto_traffic_logs upsert: %w", err)
	}
	return nil
}

func (s *MTProtoTrafficLogStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM mtproto_traffic_logs WHERE logged_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("mtproto_traffic_logs cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *MTProtoTrafficLogStore) AggregateByProfile(
	ctx context.Context, profileID, userID int, from time.Time, bucket TrafficBucket,
) ([]TrafficPoint, error) {
	var truncExpr string
	switch bucket {
	case TrafficBucket5Min:
		truncExpr = "logged_at"
	case TrafficBucketHour:
		truncExpr = "date_trunc('hour', logged_at)"
	case TrafficBucketDay:
		truncExpr = "date_trunc('day', logged_at)"
	default:
		return nil, fmt.Errorf("unknown bucket: %s", bucket)
	}

	q := fmt.Sprintf(`
		SELECT %s AS t, SUM(bytes_up)::bigint AS up, SUM(bytes_down)::bigint AS down
		FROM mtproto_traffic_logs
		WHERE profile_id = $1
		  AND profile_id IN (SELECT id FROM mtproto_profiles WHERE user_id = $2)
		  AND logged_at >= $3
		GROUP BY 1
		ORDER BY 1 ASC`, truncExpr)

	rows, err := s.pool.Query(ctx, q, profileID, userID, from)
	if err != nil {
		return nil, fmt.Errorf("mtproto traffic aggregate profile: %w", err)
	}
	defer rows.Close()

	var points []TrafficPoint
	for rows.Next() {
		var p TrafficPoint
		if err := rows.Scan(&p.Time, &p.BytesUp, &p.BytesDown); err != nil {
			return nil, err
		}
		points = append(points, p)
	}
	return points, rows.Err()
}
