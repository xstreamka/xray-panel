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

var ErrReceiptExists = errors.New("receipt already processed")

type PaymentReceipt struct {
	ID           int             `json:"id"`
	InvID        int             `json:"inv_id"`
	UserID       int             `json:"user_id"`
	PlanID       string          `json:"plan_id"`
	TariffKind   TariffKind      `json:"tariff_kind"`
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

// ApplyPayment атомарно создаёт квитанцию и применяет платёж по его виду:
//   - subscription: продлевает подписку (max(NOW(),old)+duration), обнуляет base,
//     размораживает extra, сбрасывает флаги напоминаний.
//   - addon: просто плюсует extra (только для юзеров с активной подпиской —
//     проверка должна быть на уровне webhook'а, до этого вызова).
//
// Возвращает ErrReceiptExists для повторных webhook'ов (retry) — это не ошибка,
// обработчик должен вернуть 200 OK.
func (s *PaymentReceiptStore) ApplyPayment(
	ctx context.Context, r *PaymentReceipt, tariff *Tariff,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Идемпотентность: inv_id UNIQUE → ретраи webhook'а отсекаются.
	//    tariff_kind денормализуем в колонку для удобства отчётов и фильтрации
	//    истории (subscription vs addon) без парсинга raw_payload.
	tag, err := tx.Exec(ctx,
		`INSERT INTO payment_receipts
		    (inv_id, user_id, plan_id, amount_rub, traffic_bytes, paid_at, raw_payload, tariff_kind)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (inv_id) DO NOTHING`,
		r.InvID, r.UserID, r.PlanID, r.AmountRub, r.TrafficBytes, r.PaidAt, r.RawPayload,
		string(tariff.Kind),
	)
	if err != nil {
		return fmt.Errorf("insert receipt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrReceiptExists
	}

	// Для консистентности — заполним поле и в переданной структуре,
	// чтобы вызывающий код мог использовать её после успешного Apply.
	r.TariffKind = tariff.Kind

	// 2. Применяем эффект платежа в той же транзакции.
	switch tariff.Kind {
	case TariffKindSubscription:
		ct, err := tx.Exec(ctx,
			`UPDATE users SET
			    current_tariff_id = $1,
			    tariff_expires_at = CASE
			        WHEN tariff_expires_at IS NOT NULL AND tariff_expires_at > NOW()
			            THEN tariff_expires_at + ($2 || ' days')::INTERVAL
			        ELSE NOW() + ($2 || ' days')::INTERVAL
			    END,
			    base_traffic_limit    = $3,
			    base_traffic_used     = 0,
			    extra_traffic_balance = extra_traffic_balance + frozen_extra_balance,
			    frozen_extra_balance  = 0,
			    reminder_5d_sent_at   = NULL,
			    reminder_1d_sent_at   = NULL,
			    updated_at = NOW()
			 WHERE id = $4`,
			tariff.ID, tariff.DurationDays, r.TrafficBytes, r.UserID,
		)
		if err != nil {
			return fmt.Errorf("renew subscription: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("user %d not found", r.UserID)
		}

	case TariffKindAddon:
		ct, err := tx.Exec(ctx,
			`UPDATE users SET
			    extra_traffic_balance = extra_traffic_balance + $1,
			    updated_at = NOW()
			 WHERE id = $2`,
			r.TrafficBytes, r.UserID,
		)
		if err != nil {
			return fmt.Errorf("add extra: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("user %d not found", r.UserID)
		}

	default:
		return fmt.Errorf("unknown tariff kind: %s", tariff.Kind)
	}

	return tx.Commit(ctx)
}

// ListByUser — для страницы "История пополнений"
func (s *PaymentReceiptStore) ListByUser(ctx context.Context, userID int, limit int) ([]PaymentReceipt, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, inv_id, user_id, plan_id, tariff_kind, amount_rub, traffic_bytes, paid_at, created_at
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
		// tariff_kind может быть NULL для старых квитанций, созданных до миграции
		var kind *string
		if err := rows.Scan(&r.ID, &r.InvID, &r.UserID, &r.PlanID, &kind,
			&r.AmountRub, &r.TrafficBytes, &r.PaidAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		if kind != nil {
			r.TariffKind = TariffKind(*kind)
		}
		list = append(list, r)
	}
	return list, nil
}

// ListByUserByKind — история платежей отфильтрованная по виду тарифа.
// Пригодится на странице "История" с табами subscription / addon.
func (s *PaymentReceiptStore) ListByUserByKind(
	ctx context.Context, userID int, kind TariffKind, limit int,
) ([]PaymentReceipt, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, inv_id, user_id, plan_id, tariff_kind, amount_rub, traffic_bytes, paid_at, created_at
		 FROM payment_receipts
		 WHERE user_id = $1 AND tariff_kind = $2
		 ORDER BY created_at DESC LIMIT $3`,
		userID, string(kind), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []PaymentReceipt
	for rows.Next() {
		var r PaymentReceipt
		var k *string
		if err := rows.Scan(&r.ID, &r.InvID, &r.UserID, &r.PlanID, &k,
			&r.AmountRub, &r.TrafficBytes, &r.PaidAt, &r.CreatedAt); err != nil {
			return nil, err
		}
		if k != nil {
			r.TariffKind = TariffKind(*k)
		}
		list = append(list, r)
	}
	return list, nil
}
