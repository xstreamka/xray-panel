package models

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Режимы регистрации в системе — хранятся в app_settings под ключом
// registration_mode как строка.
const (
	RegModeOpen       = "open"
	RegModeInviteOnly = "invite_only"
	RegModeBoth       = "both"
	RegModeDisabled   = "disabled"
)

// IsValidRegistrationMode — простая белая проверка, чтобы в БД не попало
// произвольное значение через форму админки.
func IsValidRegistrationMode(m string) bool {
	switch m {
	case RegModeOpen, RegModeInviteOnly, RegModeBoth, RegModeDisabled:
		return true
	}
	return false
}

// ErrInviteNotFound — инвайт не найден, либо помечен как неактивный/удалённый.
// auth-хендлер смотрит только на этот факт и показывает одну и ту же формулировку.
var ErrInviteNotFound = errors.New("invite not found")

type Invite struct {
	ID           int       `json:"id"`
	Code         string    `json:"code"`
	Note         string    `json:"note"`
	IsActive     bool      `json:"is_active"`
	IsDeleted    bool      `json:"is_deleted"`
	Clicks       int       `json:"clicks"`
	CreatedBy    *int      `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Registered   int       `json:"registered"` // юзеров по инвайту (агрегат)
	Verified     int       `json:"verified"`   // из них с подтверждённым email
	Paid         int       `json:"paid"`       // из них купивших подписку (≥1 квитанция subscription)
	CreatorLogin *string   `json:"creator_login,omitempty"`
}

type InviteStore struct {
	pool *pgxpool.Pool
}

func NewInviteStore(pool *pgxpool.Pool) *InviteStore {
	return &InviteStore{pool: pool}
}

// generateInviteCode — 16 байт энтропии в base64url (без паддинга) ≈ 22 символа.
// Безопасно для URL, достаточно длинно, чтобы не перебрать.
func generateInviteCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Create — новый активный инвайт. createdBy может быть nil (на всякий случай),
// но реально админка всегда передаёт ID текущего админа.
func (s *InviteStore) Create(ctx context.Context, note string, createdBy *int) (*Invite, error) {
	code, err := generateInviteCode()
	if err != nil {
		return nil, fmt.Errorf("generate code: %w", err)
	}
	inv := &Invite{}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO invites (code, note, created_by)
		 VALUES ($1, $2, $3)
		 RETURNING id, code, note, is_active, is_deleted, clicks, created_by, created_at, updated_at`,
		code, note, createdBy,
	).Scan(&inv.ID, &inv.Code, &inv.Note, &inv.IsActive, &inv.IsDeleted,
		&inv.Clicks, &inv.CreatedBy, &inv.CreatedAt, &inv.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert invite: %w", err)
	}
	return inv, nil
}

// GetByCode возвращает инвайт только если он «живой»: is_active=TRUE и
// is_deleted=FALSE. Остальные случаи схлопываются в ErrInviteNotFound,
// чтобы UI показывал общее сообщение «приглашение недействительно».
func (s *InviteStore) GetByCode(ctx context.Context, code string) (*Invite, error) {
	inv := &Invite{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, code, note, is_active, is_deleted, clicks, created_by, created_at, updated_at
		   FROM invites
		  WHERE code = $1 AND is_active = TRUE AND is_deleted = FALSE`,
		code,
	).Scan(&inv.ID, &inv.Code, &inv.Note, &inv.IsActive, &inv.IsDeleted,
		&inv.Clicks, &inv.CreatedBy, &inv.CreatedAt, &inv.UpdatedAt)
	if err != nil {
		return nil, ErrInviteNotFound
	}
	return inv, nil
}

// GetByID — для страницы «кто заинвайтился». Возвращает даже помеченные deleted.
func (s *InviteStore) GetByID(ctx context.Context, id int) (*Invite, error) {
	inv := &Invite{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, code, note, is_active, is_deleted, clicks, created_by, created_at, updated_at
		   FROM invites WHERE id = $1`,
		id,
	).Scan(&inv.ID, &inv.Code, &inv.Note, &inv.IsActive, &inv.IsDeleted,
		&inv.Clicks, &inv.CreatedBy, &inv.CreatedAt, &inv.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("invite not found")
	}
	return inv, nil
}

// IncrementClicks — просто +1 к счётчику переходов. Никакой дедупликации:
// пользователь явно попросил «просто клики».
func (s *InviteStore) IncrementClicks(ctx context.Context, id int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE invites SET clicks = clicks + 1, updated_at = NOW() WHERE id = $1`,
		id,
	)
	return err
}

// List — все инвайты с агрегированными счётчиками registered/verified.
// Удалённые (soft-delete) тоже отдаём: админка показывает их перечёркнутыми.
func (s *InviteStore) List(ctx context.Context) ([]Invite, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT i.id, i.code, i.note, i.is_active, i.is_deleted, i.clicks,
		        i.created_by, i.created_at, i.updated_at,
		        COALESCE(u_creator.username, NULL) AS creator_login,
		        COALESCE(agg.registered, 0) AS registered,
		        COALESCE(agg.verified, 0)   AS verified,
		        COALESCE(paid.paid, 0)      AS paid
		   FROM invites i
		   LEFT JOIN users u_creator ON u_creator.id = i.created_by
		   LEFT JOIN (
		        SELECT invite_id,
		               COUNT(*) AS registered,
		               COUNT(*) FILTER (WHERE email_verified) AS verified
		          FROM users
		         WHERE invite_id IS NOT NULL
		         GROUP BY invite_id
		   ) agg ON agg.invite_id = i.id
		   LEFT JOIN (
		        SELECT u.invite_id,
		               COUNT(DISTINCT u.id) AS paid
		          FROM users u
		          JOIN payment_receipts pr ON pr.user_id = u.id
		         WHERE u.invite_id IS NOT NULL
		           AND pr.tariff_kind = 'subscription'
		         GROUP BY u.invite_id
		   ) paid ON paid.invite_id = i.id
		  ORDER BY i.is_deleted ASC, i.id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(
			&inv.ID, &inv.Code, &inv.Note, &inv.IsActive, &inv.IsDeleted, &inv.Clicks,
			&inv.CreatedBy, &inv.CreatedAt, &inv.UpdatedAt,
			&inv.CreatorLogin,
			&inv.Registered, &inv.Verified, &inv.Paid,
		); err != nil {
			return nil, err
		}
		list = append(list, inv)
	}
	return list, nil
}

// SetActive — включить/выключить. Soft-deleted инвайт всё равно останется
// невалидным (GetByCode фильтрует по is_deleted=FALSE), так что переактивация
// удалённого ни к чему не приводит — но админке не мешает пробовать.
func (s *InviteStore) SetActive(ctx context.Context, id int, active bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE invites SET is_active = $1, updated_at = NOW() WHERE id = $2`,
		active, id,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invite %d not found", id)
	}
	return nil
}

// UpdateNote меняет только заметку. Не трогает is_active/is_deleted —
// заметку можно править даже у выключенного инвайта (для пометок «утечка»).
func (s *InviteStore) UpdateNote(ctx context.Context, id int, note string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE invites SET note = $1, updated_at = NOW() WHERE id = $2`,
		note, id,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invite %d not found", id)
	}
	return nil
}

// SoftDelete — пометить инвайт удалённым. Физически не удаляем: это бы рвало
// связь с users.invite_id и ломало страницу «кто заинвайтился».
func (s *InviteStore) SoftDelete(ctx context.Context, id int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE invites SET is_deleted = TRUE, is_active = FALSE, updated_at = NOW()
		  WHERE id = $1`,
		id,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invite %d not found", id)
	}
	return nil
}

// ListUsersByInvite — пользователи, пришедшие по конкретному инвайту.
// Нужно при расследовании утечки: открыть ссылку, увидеть всех, забанить.
func (s *InviteStore) ListUsersByInvite(ctx context.Context, inviteID int) ([]User, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+userCols+` FROM users WHERE invite_id = $1 ORDER BY id`,
		inviteID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []User
	for rows.Next() {
		var u User
		if err := scanUser(rows, &u); err != nil {
			return nil, err
		}
		list = append(list, u)
	}
	return list, nil
}

// ──────────────────────────────────────────────
//  app_settings — пока только registration_mode
// ──────────────────────────────────────────────

// GetRegistrationMode — читает текущий режим из app_settings. Если ключа нет
// (на старой БД без миграции), возвращаем open — это backward-compatible
// поведение (регистрация не сломается).
func (s *InviteStore) GetRegistrationMode(ctx context.Context) (string, error) {
	var mode string
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM app_settings WHERE key = 'registration_mode'`,
	).Scan(&mode)
	if err != nil {
		return RegModeOpen, nil
	}
	if !IsValidRegistrationMode(mode) {
		return RegModeOpen, nil
	}
	return mode, nil
}

func (s *InviteStore) SetRegistrationMode(ctx context.Context, mode string) error {
	if !IsValidRegistrationMode(mode) {
		return fmt.Errorf("invalid registration mode: %q", mode)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO app_settings (key, value) VALUES ('registration_mode', $1)
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		mode,
	)
	return err
}
