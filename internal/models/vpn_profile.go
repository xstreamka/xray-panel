package models

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type VPNProfile struct {
	ID           int        `json:"id"`
	UserID       int        `json:"user_id"`
	UUID         string     `json:"uuid"`
	Name         string     `json:"name"`
	IsActive     bool       `json:"is_active"`
	TrafficUp    int64      `json:"traffic_up"`
	TrafficDown  int64      `json:"traffic_down"`
	TrafficLimit int64      `json:"traffic_limit"`
	ExpiresAt    *time.Time `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`

	// Joined
	Username string `json:"username,omitempty"`
}

type VPNProfileStore struct {
	pool *pgxpool.Pool
}

func NewVPNProfileStore(pool *pgxpool.Pool) *VPNProfileStore {
	return &VPNProfileStore{pool: pool}
}

func (s *VPNProfileStore) Create(ctx context.Context, userID int, uuid, name string) (*VPNProfile, error) {
	p := &VPNProfile{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO vpn_profiles (user_id, uuid, name)
		 VALUES ($1, $2, $3)
		 RETURNING id, user_id, uuid, name, is_active, traffic_up, traffic_down, traffic_limit, expires_at, created_at, updated_at`,
		userID, uuid, name,
	).Scan(&p.ID, &p.UserID, &p.UUID, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create profile: %w", err)
	}
	return p, nil
}

func (s *VPNProfileStore) GetByUserID(ctx context.Context, userID int) ([]VPNProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, user_id, uuid, name, is_active, traffic_up, traffic_down, traffic_limit, expires_at, created_at, updated_at
		 FROM vpn_profiles WHERE user_id = $1 ORDER BY id`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []VPNProfile
	for rows.Next() {
		var p VPNProfile
		if err := rows.Scan(&p.ID, &p.UserID, &p.UUID, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

func (s *VPNProfileStore) GetByUUID(ctx context.Context, uuid string) (*VPNProfile, error) {
	p := &VPNProfile{}
	err := s.pool.QueryRow(ctx,
		`SELECT p.id, p.user_id, p.uuid, p.name, p.is_active, p.traffic_up, p.traffic_down, p.traffic_limit, p.expires_at, p.created_at, p.updated_at, u.username
		 FROM vpn_profiles p JOIN users u ON u.id = p.user_id
		 WHERE p.uuid = $1`,
		uuid,
	).Scan(&p.ID, &p.UserID, &p.UUID, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt, &p.Username)
	if err != nil {
		return nil, fmt.Errorf("profile not found")
	}
	return p, nil
}

func (s *VPNProfileStore) ListAll(ctx context.Context) ([]VPNProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.user_id, p.uuid, p.name, p.is_active, p.traffic_up, p.traffic_down, p.traffic_limit, p.expires_at, p.created_at, p.updated_at, u.username
		 FROM vpn_profiles p JOIN users u ON u.id = p.user_id
		 ORDER BY p.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []VPNProfile
	for rows.Next() {
		var p VPNProfile
		if err := rows.Scan(&p.ID, &p.UserID, &p.UUID, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt, &p.Username); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

func (s *VPNProfileStore) UpdateTraffic(ctx context.Context, uuid string, up, down int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE vpn_profiles SET traffic_up = traffic_up + $1, traffic_down = traffic_down + $2, updated_at = NOW()
		 WHERE uuid = $3`,
		up, down, uuid,
	)
	return err
}

func (s *VPNProfileStore) SetActive(ctx context.Context, id int, active bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE vpn_profiles SET is_active = $1, updated_at = NOW() WHERE id = $2`,
		active, id,
	)
	return err
}

func (s *VPNProfileStore) Delete(ctx context.Context, id int) (string, error) {
	var uuid string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM vpn_profiles WHERE id = $1 RETURNING uuid`, id,
	).Scan(&uuid)
	if err != nil {
		return "", fmt.Errorf("delete profile: %w", err)
	}
	return uuid, nil
}

// GetAllActiveUUIDs — для восстановления пользователей при старте Xray
func (s *VPNProfileStore) GetAllActiveUUIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT vp.uuid FROM vpn_profiles vp
		 JOIN users u ON u.id = vp.user_id
		 WHERE vp.is_active = TRUE AND u.is_active = TRUE
		   AND (vp.expires_at IS NULL OR vp.expires_at > NOW())
		   AND (vp.traffic_limit = 0 OR vp.traffic_up + vp.traffic_down < vp.traffic_limit)`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var uuids []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, err
		}
		uuids = append(uuids, uuid)
	}
	return uuids, nil
}

// GetExceeded — активные профили, превысившие лимит или с истёкшим сроком
func (s *VPNProfileStore) GetExceeded(ctx context.Context) ([]VPNProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.user_id, p.uuid, p.name, p.is_active, p.traffic_up, p.traffic_down, p.traffic_limit, p.expires_at, p.created_at, p.updated_at
		 FROM vpn_profiles p
		 WHERE p.is_active = TRUE
		   AND (
		     (p.traffic_limit > 0 AND p.traffic_up + p.traffic_down >= p.traffic_limit)
		     OR
		     (p.expires_at IS NOT NULL AND p.expires_at <= NOW())
		   )`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var profiles []VPNProfile
	for rows.Next() {
		var p VPNProfile
		if err := rows.Scan(&p.ID, &p.UserID, &p.UUID, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}
