package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrReceiptExists — квитанция с таким inv_id уже существует (повторный webhook).
var ErrReceiptExists = errors.New("receipt already processed")

type PaymentReceipt struct {
	ID           int             `json:"id"`
	InvID        int             `json:"inv_id"`
	UserID       int             `json:"user_id"`
	PlanID       string          `json:"plan_id"`
	AmountRub    float64         `json:"amount_rub"`
	TrafficBytes int64           `json:"traffic_bytes"`
	PaidAt       time.Time       `json:"paid_at"`
	RawPayload   json.RawMessage `json:"-"`
	CreatedAt    time.Time       `json:"created_at"`
}

type PaymentReceiptStore struct {
	pool *pgxpool.Pool
}

func NewPaymentReceiptStore(pool *pgxpool.Pool) *PaymentReceiptStore {
	return &PaymentReceiptStore{pool: pool}
}

// CreditBalance атомарно создаёт квитанцию и пополняет баланс пользователя.
// Возвращает ErrReceiptExists, если inv_id уже обрабатывался — это нормальный
// кейс (retry webhook), обработчик должен вернуть 200 OK.
func (s *PaymentReceiptStore) CreditBalance(ctx context.Context, r *PaymentReceipt) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`INSERT INTO payment_receipts
		    (inv_id, user_id, plan_id, amount_rub, traffic_bytes, paid_at, raw_payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (inv_id) DO NOTHING`,
		r.InvID, r.UserID, r.PlanID, r.AmountRub, r.TrafficBytes, r.PaidAt, r.RawPayload,
	)
	if err != nil {
		return fmt.Errorf("insert receipt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrReceiptExists
	}

	ct, err := tx.Exec(ctx,
		`UPDATE users
		 SET traffic_balance = traffic_balance + $1, updated_at = NOW()
		 WHERE id = $2`,
		r.TrafficBytes, r.UserID,
	)
	if err != nil {
		return fmt.Errorf("update balance: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("user %d not found", r.UserID)
	}

	return tx.Commit(ctx)
}

// ListByUser — для будущей страницы "История пополнений"
func (s *PaymentReceiptStore) ListByUser(ctx context.Context, userID int, limit int) ([]PaymentReceipt, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, inv_id, user_id, plan_id, amount_rub, traffic_bytes, paid_at, created_at
		 FROM payment_receipts WHERE user_id = $1
		 ORDER BY created_at DESC LIMIT $2`,
		userID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []PaymentReceipt
	for rows.Next() {
		var r PaymentReceipt
		if err := rows.Scan(&r.ID, &r.InvID, &r.UserID, &r.PlanID,
			&r.AmountRub, &r.TrafficBytes, &r.PaidAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, nil
}
