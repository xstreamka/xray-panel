package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// ErrInsufficientTraffic — юзеру нечего списывать (base+extra=0)
var ErrInsufficientTraffic = errors.New("insufficient traffic")

type User struct {
	ID            int        `json:"id"`
	Username      string     `json:"username"`
	Email         string     `json:"email"`
	PasswordHash  string     `json:"-"`
	IsAdmin       bool       `json:"is_admin"`
	IsActive      bool       `json:"is_active"`
	EmailVerified bool       `json:"email_verified"`
	VerifyToken   *string    `json:"-"`
	VerifyExpires *time.Time `json:"-"`

	// ===== Подписочная модель =====
	CurrentTariffID     *int       `json:"current_tariff_id"`
	TariffExpiresAt     *time.Time `json:"tariff_expires_at"`
	BaseTrafficLimit    int64      `json:"base_traffic_limit"`
	BaseTrafficUsed     int64      `json:"base_traffic_used"`
	ExtraTrafficBalance int64      `json:"extra_traffic_balance"`
	ExtraTrafficGranted int64      `json:"extra_traffic_granted"`
	FrozenExtraBalance  int64      `json:"frozen_extra_balance"`
	Reminder5dSentAt    *time.Time `json:"-"`
	Reminder1dSentAt    *time.Time `json:"-"`
	BlockNotifiedAt     *time.Time `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TotalAvailable — сколько трафика осталось списать (room в base + extra).
// Value-receiver, чтобы html/template мог вызвать метод на embedded-значении.
func (u User) TotalAvailable() int64 {
	room := u.BaseTrafficLimit - u.BaseTrafficUsed
	if room < 0 {
		room = 0
	}
	return room + u.ExtraTrafficBalance
}

// HasActiveSubscription — есть ли активный тариф по дате.
func (u User) HasActiveSubscription() bool {
	return u.TariffExpiresAt != nil && u.TariffExpiresAt.After(time.Now())
}

type UserStore struct {
	pool *pgxpool.Pool
}

func NewUserStore(pool *pgxpool.Pool) *UserStore {
	return &UserStore{pool: pool}
}

// userCols — единый список полей в нужном порядке, чтобы не дублировать SELECT-ы.
const userCols = `id, username, email, is_admin, is_active, email_verified,
                  current_tariff_id, tariff_expires_at,
                  base_traffic_limit, base_traffic_used,
                  extra_traffic_balance, extra_traffic_granted, frozen_extra_balance,
                  reminder_5d_sent_at, reminder_1d_sent_at, block_notified_at,
                  created_at, updated_at`

func scanUser(row interface {
	Scan(dest ...any) error
}, u *User) error {
	return row.Scan(
		&u.ID, &u.Username, &u.Email, &u.IsAdmin, &u.IsActive, &u.EmailVerified,
		&u.CurrentTariffID, &u.TariffExpiresAt,
		&u.BaseTrafficLimit, &u.BaseTrafficUsed,
		&u.ExtraTrafficBalance, &u.ExtraTrafficGranted, &u.FrozenExtraBalance,
		&u.Reminder5dSentAt, &u.Reminder1dSentAt, &u.BlockNotifiedAt,
		&u.CreatedAt, &u.UpdatedAt,
	)
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ──────────────────────────────────────────────
//  Регистрация / аутентификация / верификация
// ──────────────────────────────────────────────

func (s *UserStore) Create(ctx context.Context, username, email, password string) (*User, string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", fmt.Errorf("hash password: %w", err)
	}
	token, err := generateToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate token: %w", err)
	}
	expires := time.Now().Add(24 * time.Hour)

	u := &User{}
	row := s.pool.QueryRow(ctx,
		`INSERT INTO users (username, email, password_hash, email_verified, verify_token, verify_expires)
		 VALUES ($1, $2, $3, FALSE, $4, $5)
		 RETURNING `+userCols,
		username, email, string(hash), token, expires,
	)
	if err := scanUser(row, u); err != nil {
		return nil, "", fmt.Errorf("insert user: %w", err)
	}
	return u, token, nil
}

func (s *UserStore) Authenticate(ctx context.Context, username, password string) (*User, error) {
	u := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT `+userCols+`, password_hash FROM users WHERE username = $1`,
		username,
	).Scan(
		&u.ID, &u.Username, &u.Email, &u.IsAdmin, &u.IsActive, &u.EmailVerified,
		&u.CurrentTariffID, &u.TariffExpiresAt,
		&u.BaseTrafficLimit, &u.BaseTrafficUsed,
		&u.ExtraTrafficBalance, &u.ExtraTrafficGranted, &u.FrozenExtraBalance,
		&u.Reminder5dSentAt, &u.Reminder1dSentAt, &u.BlockNotifiedAt,
		&u.CreatedAt, &u.UpdatedAt,
		&u.PasswordHash,
	)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}
	if !u.IsActive {
		return nil, fmt.Errorf("user is deactivated")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, fmt.Errorf("invalid password")
	}
	return u, nil
}

func (s *UserStore) VerifyEmail(ctx context.Context, token string) (*User, error) {
	u := &User{}
	row := s.pool.QueryRow(ctx,
		`UPDATE users
		 SET email_verified = TRUE, verify_token = NULL, verify_expires = NULL, updated_at = NOW()
		 WHERE verify_token = $1 AND verify_expires > NOW() AND email_verified = FALSE
		 RETURNING `+userCols,
		token,
	)
	if err := scanUser(row, u); err != nil {
		return nil, fmt.Errorf("invalid or expired token")
	}
	return u, nil
}

func (s *UserStore) RegenerateVerifyToken(ctx context.Context, userID int) (string, string, error) {
	token, err := generateToken()
	if err != nil {
		return "", "", err
	}
	expires := time.Now().Add(24 * time.Hour)
	var email string
	err = s.pool.QueryRow(ctx,
		`UPDATE users SET verify_token = $1, verify_expires = $2, updated_at = NOW()
		 WHERE id = $3 AND email_verified = FALSE
		 RETURNING email`,
		token, expires, userID,
	).Scan(&email)
	if err != nil {
		return "", "", fmt.Errorf("user not found or already verified")
	}
	return token, email, nil
}

// ──────────────────────────────────────────────
//  Чтение
// ──────────────────────────────────────────────

func (s *UserStore) GetByID(ctx context.Context, id int) (*User, error) {
	u := &User{}
	row := s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
	if err := scanUser(row, u); err != nil {
		return nil, fmt.Errorf("user not found")
	}
	return u, nil
}

func (s *UserStore) List(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := scanUser(rows, &u); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

// ──────────────────────────────────────────────
//  Подписочная модель — основные операции
// ──────────────────────────────────────────────

// RenewSubscription продлевает или активирует подписку после успешной оплаты.
// Дата считается от max(NOW(), old_expires_at), т.е. если подписка ещё активна —
// новый срок добавляется к старому; если истекла — считаем от сегодня.
// base обнуляется и заполняется новым лимитом, frozen_extra размораживается в extra.
// Атомарно одним UPDATE.
func (s *UserStore) RenewSubscription(
	ctx context.Context,
	userID int, tariffID int, durationDays int, baseLimitBytes int64,
) (*User, error) {
	u := &User{}
	row := s.pool.QueryRow(ctx,
		`UPDATE users SET
		    current_tariff_id = $1,
		    tariff_expires_at = CASE
		        WHEN tariff_expires_at IS NOT NULL AND tariff_expires_at > NOW()
		            THEN tariff_expires_at + make_interval(days => $2)
		        ELSE NOW() + make_interval(days => $2)
		    END,
		    base_traffic_limit    = $3,
		    base_traffic_used     = 0,
		    extra_traffic_balance = extra_traffic_balance + frozen_extra_balance,
		    extra_traffic_granted = extra_traffic_balance + frozen_extra_balance,
		    frozen_extra_balance  = 0,
		    reminder_5d_sent_at   = NULL,
		    reminder_1d_sent_at   = NULL,
		    block_notified_at     = NULL,
		    updated_at = NOW()
		 WHERE id = $4
		 RETURNING `+userCols,
		tariffID, durationDays, baseLimitBytes, userID,
	)
	if err := scanUser(row, u); err != nil {
		return nil, fmt.Errorf("renew subscription: %w", err)
	}
	return u, nil
}

// AddExtra добавляет байты к активному extra-балансу (addon-тариф или подарок от админа).
// Также увеличивает granted — «сколько было выдано в этом цикле» — для прогресс-бара.
// Сбрасывает block_notified_at, чтобы при следующем исчерпании юзер получил письмо.
func (s *UserStore) AddExtra(ctx context.Context, userID int, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users
		 SET extra_traffic_balance = extra_traffic_balance + $1,
		     extra_traffic_granted = extra_traffic_granted + $1,
		     block_notified_at     = NULL,
		     updated_at = NOW()
		 WHERE id = $2`,
		bytes, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %d not found", userID)
	}
	return nil
}

// SetExtra жёстко устанавливает extra_traffic_balance заданному значению.
// Административный инструмент для ручной коррекции: +X, -X или полный сброс.
// granted синхронизируется с новым значением — бар стартует с «100% осталось».
// block_notified_at сбрасываем только если выдали трафик (bytes > 0).
func (s *UserStore) SetExtra(ctx context.Context, userID int, bytes int64) error {
	if bytes < 0 {
		bytes = 0
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE users
		 SET extra_traffic_balance = $1,
		     extra_traffic_granted = $1,
		     block_notified_at     = CASE WHEN $1 > 0 THEN NULL ELSE block_notified_at END,
		     updated_at = NOW()
		 WHERE id = $2`,
		bytes, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %d not found", userID)
	}
	return nil
}

// TryMarkBlockNotified атомарно ставит block_notified_at = NOW(), если он
// ещё NULL, и возвращает true. Если уже был проставлен — возвращает false,
// и вызывающий код должен пропустить отправку письма. Сбрасывается при
// любом пополнении (AddExtra/SetExtra/RenewSubscription/ApplyPayment).
func (s *UserStore) TryMarkBlockNotified(ctx context.Context, userID int) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET block_notified_at = NOW(), updated_at = NOW()
		 WHERE id = $1 AND block_notified_at IS NULL`,
		userID,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// CancelSubscription обнуляет подписку юзера (но не трогает extra/frozen).
// Профили отключатся автоматически в ближайшем тике stats-коллектора по
// критерию TotalAvailable <= 0, если base исчерпан. Для немедленного отключения
// вызывающий код должен сам дёрнуть DisconnectUserAll.
func (s *UserStore) CancelSubscription(ctx context.Context, userID int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET
		    current_tariff_id   = NULL,
		    tariff_expires_at   = NULL,
		    base_traffic_limit  = 0,
		    base_traffic_used   = 0,
		    reminder_5d_sent_at = NULL,
		    reminder_1d_sent_at = NULL,
		    updated_at          = NOW()
		 WHERE id = $1`,
		userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user %d not found", userID)
	}
	return nil
}

// DeductTraffic атомарно списывает дельту: сначала заполняет base_used до лимита,
// остаток снимает с extra. Возвращает обновлённого юзера и флаг «исчерпан» —
// если remaining <= 0, вызывающий код должен отключить все профили юзера.
//
// SQL-трюк: в Postgres все выражения в SET видят ОРИГИНАЛЬНЫЕ значения строки,
// поэтому extra вычисляется от старого base_used корректно.
func (s *UserStore) DeductTraffic(
	ctx context.Context, userID int, bytes int64,
) (remaining int64, err error) {
	if bytes <= 0 {
		u, err := s.GetByID(ctx, userID)
		if err != nil {
			return 0, err
		}
		return u.TotalAvailable(), nil
	}
	err = s.pool.QueryRow(ctx,
		`UPDATE users SET
		    base_traffic_used     = LEAST(base_traffic_limit, base_traffic_used + $1),
		    extra_traffic_balance = GREATEST(0,
		        extra_traffic_balance
		        - GREATEST(0, base_traffic_used + $1 - base_traffic_limit)
		    ),
		    updated_at = NOW()
		 WHERE id = $2
		 RETURNING
		    (base_traffic_limit - base_traffic_used) + extra_traffic_balance`,
		bytes, userID,
	).Scan(&remaining)
	if err != nil {
		return 0, fmt.Errorf("deduct traffic: %w", err)
	}
	return remaining, nil
}

// ExpireSubscriptions обрабатывает всех юзеров, у которых срок тарифа истёк:
// замораживает их extra, обнуляет base. Возвращает список userID, которым теперь
// надо отключить VPN-профили (их дальше обрабатывает Xray-клиент).
//
// Запускается кроном. Идемпотентно: повторный вызов ничего не изменит,
// т.к. после первого прохода tariff_expires_at будет NULL.
func (s *UserStore) ExpireSubscriptions(ctx context.Context) ([]int, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE users SET
		    frozen_extra_balance  = frozen_extra_balance + extra_traffic_balance,
		    extra_traffic_balance = 0,
		    extra_traffic_granted = 0,
		    base_traffic_limit    = 0,
		    base_traffic_used     = 0,
		    current_tariff_id     = NULL,
		    tariff_expires_at     = NULL,
		    updated_at = NOW()
		 WHERE tariff_expires_at IS NOT NULL
		   AND tariff_expires_at <= NOW()
		 RETURNING id`,
	)
	if err != nil {
		return nil, fmt.Errorf("expire subscriptions: %w", err)
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
	return ids, nil
}

// UsersForReminder возвращает юзеров, которым надо послать письмо о скором истечении.
// days — за сколько дней напоминаем (5 или 1). Флаг сброса отправки — внутри column.
// После отправки вызвать MarkReminderSent.
func (s *UserStore) UsersForReminder(ctx context.Context, days int) ([]User, error) {
	var column string
	switch days {
	case 5:
		column = "reminder_5d_sent_at"
	case 1:
		column = "reminder_1d_sent_at"
	default:
		return nil, fmt.Errorf("unsupported reminder: %d days", days)
	}
	q := `SELECT ` + userCols + ` FROM users
	      WHERE tariff_expires_at IS NOT NULL
	        AND tariff_expires_at BETWEEN NOW() AND NOW() + make_interval(days => $1)
	        AND ` + column + ` IS NULL
	        AND is_active = TRUE`
	rows, err := s.pool.Query(ctx, q, days)
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

// MarkReminderSent ставит timestamp, чтобы не слать повторно.
func (s *UserStore) MarkReminderSent(ctx context.Context, userID int, days int) error {
	var column string
	switch days {
	case 5:
		column = "reminder_5d_sent_at"
	case 1:
		column = "reminder_1d_sent_at"
	default:
		return fmt.Errorf("unsupported reminder: %d days", days)
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET `+column+` = NOW(), updated_at = NOW() WHERE id = $1`,
		userID,
	)
	return err
}

