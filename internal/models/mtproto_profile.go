package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type MTProtoProfile struct {
	ID           int        `json:"id"`
	UserID       int        `json:"user_id"`
	SecretHex    string     `json:"secret_hex"`
	Name         string     `json:"name"`
	IsActive     bool       `json:"is_active"`
	TrafficUp    int64      `json:"traffic_up"`
	TrafficDown  int64      `json:"traffic_down"`
	TrafficLimit int64      `json:"traffic_limit"`
	ExpiresAt    *time.Time `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`

	Username string `json:"username,omitempty"`
}

func (p MTProtoProfile) MetricName() string {
	return fmt.Sprintf("mtproto_%d", p.ID)
}

func GenerateMTProtoSecretHex() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type MTProtoProfileStore struct {
	pool *pgxpool.Pool
}

func NewMTProtoProfileStore(pool *pgxpool.Pool) *MTProtoProfileStore {
	return &MTProtoProfileStore{pool: pool}
}

func (s *MTProtoProfileStore) Create(ctx context.Context, userID int, secretHex, name string) (*MTProtoProfile, error) {
	p := &MTProtoProfile{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO mtproto_profiles (user_id, secret_hex, name)
		 VALUES ($1, $2, $3)
		 RETURNING id, user_id, secret_hex, name, is_active, traffic_up, traffic_down, traffic_limit, expires_at, created_at, updated_at`,
		userID, secretHex, name,
	).Scan(&p.ID, &p.UserID, &p.SecretHex, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create mtproto profile: %w", err)
	}
	return p, nil
}

func (s *MTProtoProfileStore) GetByUserID(ctx context.Context, userID int) ([]MTProtoProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, secret_hex, name, is_active, traffic_up, traffic_down, traffic_limit, expires_at, created_at, updated_at
		 FROM mtproto_profiles WHERE user_id = $1 ORDER BY id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []MTProtoProfile
	for rows.Next() {
		var p MTProtoProfile
		if err := rows.Scan(&p.ID, &p.UserID, &p.SecretHex, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

func (s *MTProtoProfileStore) GetByID(ctx context.Context, id int) (*MTProtoProfile, error) {
	p := &MTProtoProfile{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, secret_hex, name, is_active, traffic_up, traffic_down, traffic_limit, expires_at, created_at, updated_at
		 FROM mtproto_profiles WHERE id = $1`,
		id,
	).Scan(&p.ID, &p.UserID, &p.SecretHex, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("mtproto profile not found")
	}
	return p, nil
}

func (s *MTProtoProfileStore) ListAll(ctx context.Context) ([]MTProtoProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.user_id, p.secret_hex, p.name, p.is_active, p.traffic_up, p.traffic_down, p.traffic_limit, p.expires_at, p.created_at, p.updated_at, u.username
		 FROM mtproto_profiles p JOIN users u ON u.id = p.user_id
		 ORDER BY p.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []MTProtoProfile
	for rows.Next() {
		var p MTProtoProfile
		if err := rows.Scan(&p.ID, &p.UserID, &p.SecretHex, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt, &p.Username); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

func (s *MTProtoProfileStore) GetAllActive(ctx context.Context) ([]MTProtoProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.user_id, p.secret_hex, p.name, p.is_active, p.traffic_up, p.traffic_down, p.traffic_limit, p.expires_at, p.created_at, p.updated_at, u.username
		 FROM mtproto_profiles p
		 JOIN users u ON u.id = p.user_id
		 WHERE p.is_active = TRUE
		   AND u.is_active = TRUE
		   AND (p.expires_at IS NULL OR p.expires_at > NOW())
		   AND (u.base_traffic_limit - u.base_traffic_used) + u.extra_traffic_balance > 0
		   AND (p.traffic_limit = 0 OR p.traffic_up + p.traffic_down < p.traffic_limit)
		 ORDER BY p.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []MTProtoProfile
	for rows.Next() {
		var p MTProtoProfile
		if err := rows.Scan(&p.ID, &p.UserID, &p.SecretHex, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt, &p.Username); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

func (s *MTProtoProfileStore) UpdateTrafficByID(ctx context.Context, id int, up, down int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mtproto_profiles SET traffic_up = traffic_up + $1, traffic_down = traffic_down + $2, updated_at = NOW()
		 WHERE id = $3`,
		up, down, id,
	)
	return err
}

func (s *MTProtoProfileStore) SetActive(ctx context.Context, id int, active bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mtproto_profiles
		 SET is_active         = $1,
		     limit_notified_at = CASE WHEN $1 THEN NULL ELSE limit_notified_at END,
		     updated_at        = NOW()
		 WHERE id = $2`,
		active, id,
	)
	return err
}

func (s *MTProtoProfileStore) Delete(ctx context.Context, id int) (string, error) {
	var secret string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM mtproto_profiles WHERE id = $1 RETURNING secret_hex`, id,
	).Scan(&secret)
	if err != nil {
		return "", fmt.Errorf("delete mtproto profile: %w", err)
	}
	return secret, nil
}

func (s *MTProtoProfileStore) SetLimit(ctx context.Context, id int, limitBytes int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mtproto_profiles
		 SET traffic_limit     = $1,
		     limit_notified_at = NULL,
		     updated_at        = NOW()
		 WHERE id = $2`,
		limitBytes, id,
	)
	return err
}

func (s *MTProtoProfileStore) ResetTraffic(ctx context.Context, id int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE mtproto_profiles
		 SET traffic_up        = 0,
		     traffic_down      = 0,
		     limit_notified_at = NULL,
		     updated_at        = NOW()
		 WHERE id = $1`, id,
	)
	return err
}

func (s *MTProtoProfileStore) TryMarkLimitNotified(ctx context.Context, id int) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE mtproto_profiles SET limit_notified_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND limit_notified_at IS NULL`,
		id,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *MTProtoProfileStore) DeactivateAllByUser(ctx context.Context, userID int) ([]int, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE mtproto_profiles SET is_active = FALSE, updated_at = NOW()
		 WHERE user_id = $1 AND is_active = TRUE
		 RETURNING id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *MTProtoProfileStore) ReactivateAllByUser(ctx context.Context, userID int) ([]int, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE mtproto_profiles
		 SET is_active         = TRUE,
		     limit_notified_at = NULL,
		     updated_at        = NOW()
		 WHERE user_id = $1 AND is_active = FALSE
		 RETURNING id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *MTProtoProfileStore) GetExceeded(ctx context.Context) ([]MTProtoProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.user_id, p.secret_hex, p.name, p.is_active,
		        p.traffic_up, p.traffic_down, p.traffic_limit, p.expires_at,
		        p.created_at, p.updated_at
		 FROM mtproto_profiles p
		 JOIN users u ON u.id = p.user_id
		 WHERE p.is_active = TRUE
		   AND (
		     (p.expires_at IS NOT NULL AND p.expires_at <= NOW())
		     OR (u.base_traffic_limit - u.base_traffic_used) + u.extra_traffic_balance <= 0
		     OR (p.traffic_limit > 0 AND p.traffic_up + p.traffic_down >= p.traffic_limit)
		   )`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []MTProtoProfile
	for rows.Next() {
		var p MTProtoProfile
		if err := rows.Scan(&p.ID, &p.UserID, &p.SecretHex, &p.Name, &p.IsActive,
			&p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt,
			&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}
