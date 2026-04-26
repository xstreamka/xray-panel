package models

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TariffKind — subscription даёт пакет с датой истечения,
// addon — просто докупка трафика к активной подписке.
type TariffKind string

const (
	TariffKindSubscription TariffKind = "subscription"
	TariffKindAddon        TariffKind = "addon"
)

type Tariff struct {
	ID           int        `json:"id"`
	Code         string     `json:"code"`
	Label        string     `json:"label"`
	Description  string     `json:"description"`
	AmountRub    float64    `json:"amount_rub"`
	TrafficGB    float64    `json:"traffic_gb"`
	IsPopular    bool       `json:"is_popular"`
	IsDiscount   bool       `json:"is_discount"`
	IsActive     bool       `json:"is_active"`
	SortOrder    int        `json:"sort_order"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	DurationDays int        `json:"duration_days"`
	Kind         TariffKind `json:"kind"`
}

type TariffStore struct {
	pool *pgxpool.Pool
}

func NewTariffStore(pool *pgxpool.Pool) *TariffStore {
	return &TariffStore{pool: pool}
}

const tariffCols = `id, code, label, description, amount_rub, traffic_gb,
                    duration_days, kind,
                    is_popular, is_discount, is_active, sort_order, created_at, updated_at`

func scanTariff(row interface {
	Scan(dest ...any) error
}, t *Tariff) error {
	return row.Scan(&t.ID, &t.Code, &t.Label, &t.Description,
		&t.AmountRub, &t.TrafficGB,
		&t.DurationDays, &t.Kind,
		&t.IsPopular, &t.IsDiscount, &t.IsActive,
		&t.SortOrder, &t.CreatedAt, &t.UpdatedAt)
}

// ListActive — для страницы /pay (только активные, в порядке sort_order)
func (s *TariffStore) ListActive(ctx context.Context) ([]Tariff, error) {
	return s.list(ctx, true)
}

// ListAll — для админки (и отключённые тоже)
func (s *TariffStore) ListAll(ctx context.Context) ([]Tariff, error) {
	return s.list(ctx, false)
}

func (s *TariffStore) list(ctx context.Context, activeOnly bool) ([]Tariff, error) {
	q := `SELECT ` + tariffCols + ` FROM tariffs`
	if activeOnly {
		q += ` WHERE is_active = TRUE`
	}
	q += ` ORDER BY sort_order, id`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list tariffs: %w", err)
	}
	defer rows.Close()

	var list []Tariff
	for rows.Next() {
		var t Tariff
		if err := scanTariff(rows, &t); err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, nil
}

// GetByCode — PayHandler.Checkout ищет тариф по plan_id из формы
func (s *TariffStore) GetByCode(ctx context.Context, code string) (*Tariff, error) {
	t := &Tariff{}
	row := s.pool.QueryRow(ctx,
		`SELECT `+tariffCols+` FROM tariffs WHERE code = $1`, code)
	if err := scanTariff(row, t); err != nil {
		return nil, fmt.Errorf("tariff not found")
	}
	return t, nil
}

func (s *TariffStore) Create(ctx context.Context, t *Tariff) error {
	if t.Kind == "" {
		t.Kind = TariffKindSubscription
	}
	return s.pool.QueryRow(ctx,
		`INSERT INTO tariffs (code, label, description, amount_rub, traffic_gb,
                      duration_days, kind,
                      is_popular, is_discount, is_active, sort_order)
 			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
 			RETURNING id, created_at, updated_at`,
		t.Code, t.Label, t.Description, t.AmountRub, t.TrafficGB,
		t.DurationDays, string(t.Kind),
		t.IsPopular, t.IsDiscount, t.IsActive, t.SortOrder,
	).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
}

func (s *TariffStore) Update(ctx context.Context, t *Tariff) error {
	if t.Kind == "" {
		t.Kind = TariffKindSubscription
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tariffs
 				SET code=$1, label=$2, description=$3, amount_rub=$4, traffic_gb=$5,
     			duration_days=$6, kind=$7,
     			is_popular=$8, is_discount=$9, is_active=$10, sort_order=$11, updated_at=NOW()
 			WHERE id=$12`,
		t.Code, t.Label, t.Description, t.AmountRub, t.TrafficGB,
		t.DurationDays, string(t.Kind),
		t.IsPopular, t.IsDiscount, t.IsActive, t.SortOrder, t.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("tariff not found")
	}
	return nil
}

func (s *TariffStore) Delete(ctx context.Context, id int) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM tariffs WHERE id=$1`, id)
	return err
}

// ListActiveByKind — только активные тарифы указанного вида,
// для фильтрации UI: subscription-карточки отдельно от addon-карточек.
func (s *TariffStore) ListActiveByKind(ctx context.Context, kind TariffKind) ([]Tariff, error) {
	q := `SELECT ` + tariffCols + ` FROM tariffs
	      WHERE is_active = TRUE AND kind = $1
	      ORDER BY sort_order, id`
	rows, err := s.pool.Query(ctx, q, string(kind))
	if err != nil {
		return nil, fmt.Errorf("list tariffs by kind: %w", err)
	}
	defer rows.Close()

	var list []Tariff
	for rows.Next() {
		var t Tariff
		if err := scanTariff(rows, &t); err != nil {
			return nil, err
		}
		list = append(list, t)
	}
	return list, nil
}

// GetByID — нужен дашборду для показа имени текущего тарифа пользователя.
func (s *TariffStore) GetByID(ctx context.Context, id int) (*Tariff, error) {
	t := &Tariff{}
	row := s.pool.QueryRow(ctx,
		`SELECT `+tariffCols+` FROM tariffs WHERE id = $1`, id)
	if err := scanTariff(row, t); err != nil {
		return nil, fmt.Errorf("tariff not found")
	}
	return t, nil
}
