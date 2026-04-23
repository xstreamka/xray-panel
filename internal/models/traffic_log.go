package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TrafficLog — один 5-минутный бакет трафика профиля. Писатель — коллектор,
// раз в минуту батчем UPSERT-ит накопленные в памяти дельты. Читатели пока
// не нужны (добавим вместе с графиками в дашборде).
type TrafficLog struct {
	ProfileID int       `json:"profile_id"`
	LoggedAt  time.Time `json:"logged_at"`
	BytesUp   int64     `json:"bytes_up"`
	BytesDown int64     `json:"bytes_down"`
}

type TrafficLogStore struct {
	pool *pgxpool.Pool
}

func NewTrafficLogStore(pool *pgxpool.Pool) *TrafficLogStore {
	return &TrafficLogStore{pool: pool}
}

// TrafficBucketSize — размер одного агрегационного бакета. Коллектор
// выравнивает logged_at на ближайшую кратную границу (см. BucketTime).
const TrafficBucketSize = 5 * time.Minute

// BucketTime выравнивает t вниз до границы 5-минутного бакета. Нужен, чтобы
// все писатели бились в одну и ту же строку по ON CONFLICT независимо от того,
// в какую секунду внутри интервала пришёл flush.
func BucketTime(t time.Time) time.Time {
	return t.UTC().Truncate(TrafficBucketSize)
}

// UpsertBatch вставляет/суммирует дельты за текущий бакет одним запросом.
// Вызывается из коллектора раз в минуту; entries приходят уже агрегированные
// в памяти за период. Пустой batch — no-op.
func (s *TrafficLogStore) UpsertBatch(ctx context.Context, bucket time.Time, entries map[int][2]int64) error {
	if len(entries) == 0 {
		return nil
	}

	// Строим VALUES (...), (...), ... — pgx не даёт нативного batch-UPSERT,
	// а CopyFrom не умеет ON CONFLICT. Параметров <= 4×N, с лимитом Postgres
	// 65535 это потолок ~16k профилей за один вызов — нам хватит.
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

	q := `INSERT INTO traffic_logs (profile_id, logged_at, bytes_up, bytes_down) VALUES ` +
		strings.Join(placeholders, ",") +
		` ON CONFLICT (profile_id, logged_at) DO UPDATE SET
		   bytes_up   = traffic_logs.bytes_up   + EXCLUDED.bytes_up,
		   bytes_down = traffic_logs.bytes_down + EXCLUDED.bytes_down`

	_, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("traffic_logs upsert: %w", err)
	}
	return nil
}

// DeleteOlderThan удаляет бакеты старше cutoff. Retention-джоб в коллекторе
// зовёт это раз в сутки. Возвращает число удалённых строк для лога.
func (s *TrafficLogStore) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM traffic_logs WHERE logged_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("traffic_logs cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RangeByProfile — будущий ридер для графиков. Возвращает бакеты одного
// профиля за диапазон, упорядоченные по времени. Пока не используется.
func (s *TrafficLogStore) RangeByProfile(ctx context.Context, profileID int, from, to time.Time) ([]TrafficLog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT profile_id, logged_at, bytes_up, bytes_down
		FROM traffic_logs
		WHERE profile_id = $1 AND logged_at >= $2 AND logged_at < $3
		ORDER BY logged_at ASC`,
		profileID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return pgx.CollectRows(rows, pgx.RowToStructByName[TrafficLog])
}
