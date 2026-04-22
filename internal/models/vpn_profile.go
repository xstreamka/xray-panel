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

// GetAllActiveUUIDs — UUID профилей, которые должны быть ВКЛЮЧЕНЫ в Xray.
//
// Условия активности:
//   - profile.is_active = TRUE
//   - user.is_active = TRUE
//   - expires_at NULL или в будущем
//   - у юзера есть доступный трафик: (base_limit - base_used) + extra > 0
//   - если у профиля задан personal limit (traffic_limit > 0) — не превышен
func (s *VPNProfileStore) GetAllActiveUUIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.uuid FROM vpn_profiles p
		 JOIN users u ON u.id = p.user_id
		 WHERE p.is_active = TRUE
		   AND u.is_active = TRUE
		   AND (p.expires_at IS NULL OR p.expires_at > NOW())
		   AND (u.base_traffic_limit - u.base_traffic_used) + u.extra_traffic_balance > 0
		   AND (p.traffic_limit = 0 OR p.traffic_up + p.traffic_down < p.traffic_limit)`,
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

// GetAllInactiveUUIDs — UUID профилей, которые должны быть ОТКЛЮЧЕНЫ.
// Симметрично GetAllActiveUUIDs, для sync при старте Xray.
func (s *VPNProfileStore) GetAllInactiveUUIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.uuid FROM vpn_profiles p
		 JOIN users u ON u.id = p.user_id
		 WHERE p.is_active = FALSE
		    OR u.is_active = FALSE
		    OR (p.expires_at IS NOT NULL AND p.expires_at <= NOW())
		    OR (u.base_traffic_limit - u.base_traffic_used) + u.extra_traffic_balance <= 0
		    OR (p.traffic_limit > 0 AND p.traffic_up + p.traffic_down >= p.traffic_limit)`,
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

// GetExceeded переработан. Теперь возвращает профили, которые СЕЙЧАС активны
// в БД, но должны быть отключены. Используется stats.go для выявления того,
// кого пора вырубить через gRPC.
func (s *VPNProfileStore) GetExceeded(ctx context.Context) ([]VPNProfile, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.user_id, p.uuid, p.name, p.is_active,
		        p.traffic_up, p.traffic_down, p.traffic_limit, p.expires_at,
		        p.created_at, p.updated_at
		 FROM vpn_profiles p
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
	var profiles []VPNProfile
	for rows.Next() {
		var p VPNProfile
		if err := rows.Scan(&p.ID, &p.UserID, &p.UUID, &p.Name, &p.IsActive,
			&p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt,
			&p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}
	return profiles, nil
}

func (s *VPNProfileStore) GetByID(ctx context.Context, id int) (*VPNProfile, error) {
	p := &VPNProfile{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, uuid, name, is_active, traffic_up, traffic_down, traffic_limit, expires_at, created_at, updated_at
		 FROM vpn_profiles WHERE id = $1`, id,
	).Scan(&p.ID, &p.UserID, &p.UUID, &p.Name, &p.IsActive, &p.TrafficUp, &p.TrafficDown, &p.TrafficLimit, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("profile not found")
	}
	return p, nil
}

func (s *VPNProfileStore) SetLimit(ctx context.Context, id int, limitBytes int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE vpn_profiles SET traffic_limit = $1, updated_at = NOW() WHERE id = $2`,
		limitBytes, id,
	)
	return err
}

func (s *VPNProfileStore) ResetTraffic(ctx context.Context, id int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE vpn_profiles SET traffic_up = 0, traffic_down = 0, updated_at = NOW() WHERE id = $1`, id,
	)
	return err
}

// DeactivateAllByUser помечает все активные профили юзера как inactive в БД.
// Это не отключает их в Xray — отключение gRPC-вызовом делает stats-коллектор.
// Возвращает список UUID, которые были отключены.
func (s *VPNProfileStore) DeactivateAllByUser(ctx context.Context, userID int) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE vpn_profiles SET is_active = FALSE, updated_at = NOW()
		 WHERE user_id = $1 AND is_active = TRUE
		 RETURNING uuid`,
		userID,
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

// ReactivateAllByUser включает все профили юзера (после успешного продления).
// Сбрасывает их персональные счётчики? — НЕТ. traffic_up/down считаются кумулятивно
// за всё время жизни профиля, сброс только через админку вручную.
func (s *VPNProfileStore) ReactivateAllByUser(ctx context.Context, userID int) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE vpn_profiles SET is_active = TRUE, updated_at = NOW()
		 WHERE user_id = $1 AND is_active = FALSE
		 RETURNING uuid`,
		userID,
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
