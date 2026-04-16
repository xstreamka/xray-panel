package models

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Tariff struct {
	ID          int       `json:"id"`
	Code        string    `json:"code"`
	Label       string    `json:"label"`
	Description string    `json:"description"`
	AmountRub   float64   `json:"amount_rub"`
	TrafficGB   float64   `json:"traffic_gb"`
	IsPopular   bool      `json:"is_popular"`
	IsActive    bool      `json:"is_active"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type TariffStore struct {
	pool *pgxpool.Pool
}

func NewTariffStore(pool *pgxpool.Pool) *TariffStore {
	return &TariffStore{pool: pool}
}

const tariffCols = `id, code, label, description, amount_rub, traffic_gb,
                    is_popular, is_active, sort_order, created_at, updated_at`

func scanTariff(row interface {
	Scan(dest ...any) error
}, t *Tariff) error {
	return row.Scan(&t.ID, &t.Code, &t.Label, &t.Description,
		&t.AmountRub, &t.TrafficGB, &t.IsPopular, &t.IsActive,
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
	return s.pool.QueryRow(ctx,
		`INSERT INTO tariffs (code, label, description, amount_rub, traffic_gb, is_popular, is_active, sort_order)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at, updated_at`,
		t.Code, t.Label, t.Description, t.AmountRub, t.TrafficGB,
		t.IsPopular, t.IsActive, t.SortOrder,
	).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)
}

func (s *TariffStore) Update(ctx context.Context, t *Tariff) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tariffs
		 SET code=$1, label=$2, description=$3, amount_rub=$4, traffic_gb=$5,
		     is_popular=$6, is_active=$7, sort_order=$8, updated_at=NOW()
		 WHERE id=$9`,
		t.Code, t.Label, t.Description, t.AmountRub, t.TrafficGB,
		t.IsPopular, t.IsActive, t.SortOrder, t.ID)
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
