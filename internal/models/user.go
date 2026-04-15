package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID             int        `json:"id"`
	Username       string     `json:"username"`
	Email          string     `json:"email"`
	PasswordHash   string     `json:"-"`
	IsAdmin        bool       `json:"is_admin"`
	IsActive       bool       `json:"is_active"`
	EmailVerified  bool       `json:"email_verified"`
	VerifyToken    *string    `json:"-"`
	VerifyExpires  *time.Time `json:"-"`
	TrafficBalance int64      `json:"traffic_balance"` // байты — неиспользованный баланс
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type UserStore struct {
	pool *pgxpool.Pool
}

func NewUserStore(pool *pgxpool.Pool) *UserStore {
	return &UserStore{pool: pool}
}

// generateToken генерирует случайный hex-токен (32 байта = 64 hex символа)
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *UserStore) Create(ctx context.Context, username, email, password string) (*User, string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", fmt.Errorf("hash password: %w", err)
	}

	token, err := generateToken()
	if err != nil {
		return nil, "", fmt.Errorf("generate token: %w", err)
	}

	expires := time.Now().Add(24 * time.Hour) // токен живёт 24 часа

	user := &User{}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (username, email, password_hash, email_verified, verify_token, verify_expires, traffic_balance)
		 VALUES ($1, $2, $3, FALSE, $4, $5, 0)
		 RETURNING id, username, email, is_admin, is_active, email_verified, traffic_balance, created_at, updated_at`,
		username, email, string(hash), token, expires,
	).Scan(&user.ID, &user.Username, &user.Email, &user.IsAdmin, &user.IsActive, &user.EmailVerified, &user.TrafficBalance, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return nil, "", fmt.Errorf("insert user: %w", err)
	}

	return user, token, nil
}

func (s *UserStore) Authenticate(ctx context.Context, username, password string) (*User, error) {
	user := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, email, password_hash, is_admin, is_active, email_verified, traffic_balance, created_at, updated_at
		 FROM users WHERE username = $1`,
		username,
	).Scan(&user.ID, &user.Username, &user.Email, &user.PasswordHash, &user.IsAdmin, &user.IsActive, &user.EmailVerified, &user.TrafficBalance, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}

	if !user.IsActive {
		return nil, fmt.Errorf("user is deactivated")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return nil, fmt.Errorf("invalid password")
	}

	return user, nil
}

// VerifyEmail подтверждает email по токену. Возвращает user или ошибку.
func (s *UserStore) VerifyEmail(ctx context.Context, token string) (*User, error) {
	user := &User{}
	err := s.pool.QueryRow(ctx,
		`UPDATE users
		 SET email_verified = TRUE, verify_token = NULL, verify_expires = NULL, updated_at = NOW()
		 WHERE verify_token = $1 AND verify_expires > NOW() AND email_verified = FALSE
		 RETURNING id, username, email, is_admin, is_active, email_verified, traffic_balance, created_at, updated_at`,
		token,
	).Scan(&user.ID, &user.Username, &user.Email, &user.IsAdmin, &user.IsActive, &user.EmailVerified, &user.TrafficBalance, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid or expired token")
	}
	return user, nil
}

// RegenerateVerifyToken создаёт новый токен (для повторной отправки)
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

func (s *UserStore) GetByID(ctx context.Context, id int) (*User, error) {
	user := &User{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, username, email, is_admin, is_active, email_verified, traffic_balance, created_at, updated_at
		 FROM users WHERE id = $1`,
		id,
	).Scan(&user.ID, &user.Username, &user.Email, &user.IsAdmin, &user.IsActive, &user.EmailVerified, &user.TrafficBalance, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("user not found")
	}
	return user, nil
}

func (s *UserStore) List(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, username, email, is_admin, is_active, email_verified, traffic_balance, created_at, updated_at
		 FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.IsAdmin, &u.IsActive, &u.EmailVerified, &u.TrafficBalance, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

// AddBalance добавляет байты к балансу (после оплаты)
func (s *UserStore) AddBalance(ctx context.Context, userID int, bytes int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET traffic_balance = traffic_balance + $1, updated_at = NOW() WHERE id = $2`,
		bytes, userID,
	)
	return err
}

// DeductBalance списывает байты с баланса (при создании профиля). Возвращает ошибку если не хватает.
func (s *UserStore) DeductBalance(ctx context.Context, userID int, bytes int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET traffic_balance = traffic_balance - $1, updated_at = NOW()
		 WHERE id = $2 AND traffic_balance >= $1`,
		bytes, userID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("insufficient balance")
	}
	return nil
}

// RefundBalance возвращает неиспользованный трафик на баланс (при удалении профиля)
func (s *UserStore) RefundBalance(ctx context.Context, userID int, bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET traffic_balance = traffic_balance + $1, updated_at = NOW() WHERE id = $2`,
		bytes, userID,
	)
	return err
}

// GetBalance возвращает текущий баланс пользователя
func (s *UserStore) GetBalance(ctx context.Context, userID int) (int64, error) {
	var balance int64
	err := s.pool.QueryRow(ctx,
		`SELECT traffic_balance FROM users WHERE id = $1`, userID,
	).Scan(&balance)
	return balance, err
}
