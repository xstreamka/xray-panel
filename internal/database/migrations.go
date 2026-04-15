package database

import (
	"context"
	"fmt"
	"log"
)

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS users (
		id            SERIAL PRIMARY KEY,
		username      VARCHAR(100) UNIQUE NOT NULL,
		email         VARCHAR(255) UNIQUE NOT NULL,
		password_hash VARCHAR(255) NOT NULL,
		is_admin      BOOLEAN DEFAULT FALSE,
		is_active     BOOLEAN DEFAULT TRUE,
		email_verified BOOLEAN DEFAULT FALSE,
		verify_token  VARCHAR(64),
		verify_expires TIMESTAMPTZ,
		traffic_balance BIGINT DEFAULT 0,
		created_at    TIMESTAMPTZ DEFAULT NOW(),
		updated_at    TIMESTAMPTZ DEFAULT NOW()
	)`,

	`CREATE TABLE IF NOT EXISTS vpn_profiles (
		id           SERIAL PRIMARY KEY,
		user_id      INTEGER REFERENCES users(id) ON DELETE CASCADE,
		uuid         VARCHAR(36) UNIQUE NOT NULL,
		name         VARCHAR(100) NOT NULL DEFAULT 'default',
		is_active    BOOLEAN DEFAULT TRUE,
		traffic_up   BIGINT DEFAULT 0,
		traffic_down BIGINT DEFAULT 0,
		traffic_limit BIGINT DEFAULT 0,
		expires_at   TIMESTAMPTZ,
		created_at   TIMESTAMPTZ DEFAULT NOW(),
		updated_at   TIMESTAMPTZ DEFAULT NOW(),
		UNIQUE(user_id, name)
	)`,

	`CREATE TABLE IF NOT EXISTS traffic_logs (
		id         SERIAL PRIMARY KEY,
		profile_id INTEGER REFERENCES vpn_profiles(id) ON DELETE CASCADE,
		bytes_up   BIGINT NOT NULL DEFAULT 0,
		bytes_down BIGINT NOT NULL DEFAULT 0,
		logged_at  TIMESTAMPTZ DEFAULT NOW()
	)`,

	`CREATE INDEX IF NOT EXISTS idx_traffic_logs_profile_id ON traffic_logs(profile_id)`,
	`CREATE INDEX IF NOT EXISTS idx_traffic_logs_logged_at ON traffic_logs(logged_at)`,
	`CREATE INDEX IF NOT EXISTS idx_vpn_profiles_user_id ON vpn_profiles(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_vpn_profiles_uuid ON vpn_profiles(uuid)`,

	// --- Миграции для существующих БД (ALTER TABLE) ---
	// email_verified + verify_token + verify_expires
	`DO $$ BEGIN
		ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified BOOLEAN DEFAULT FALSE;
		ALTER TABLE users ADD COLUMN IF NOT EXISTS verify_token VARCHAR(64);
		ALTER TABLE users ADD COLUMN IF NOT EXISTS verify_expires TIMESTAMPTZ;
	EXCEPTION WHEN others THEN NULL;
	END $$`,

	// traffic_balance — баланс гигабайт (в байтах)
	`DO $$ BEGIN
		ALTER TABLE users ADD COLUMN IF NOT EXISTS traffic_balance BIGINT DEFAULT 0;
	EXCEPTION WHEN others THEN NULL;
	END $$`,

	`CREATE INDEX IF NOT EXISTS idx_users_verify_token ON users(verify_token) WHERE verify_token IS NOT NULL`,

	// Таблица платежей (пригодится для Robokassa/Enot.io)
	`CREATE TABLE IF NOT EXISTS payments (
		id          SERIAL PRIMARY KEY,
		user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
		amount_rub  NUMERIC(10,2) NOT NULL,
		bytes_added BIGINT NOT NULL,
		status      VARCHAR(20) NOT NULL DEFAULT 'pending',
		provider    VARCHAR(50),
		external_id VARCHAR(255),
		created_at  TIMESTAMPTZ DEFAULT NOW(),
		completed_at TIMESTAMPTZ
	)`,

	`CREATE INDEX IF NOT EXISTS idx_payments_user_id ON payments(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_payments_external_id ON payments(external_id)`,
}

func (db *DB) Migrate() error {
	ctx := context.Background()

	for i, m := range migrations {
		if _, err := db.Pool.Exec(ctx, m); err != nil {
			return fmt.Errorf("migration %d failed: %w", i, err)
		}
	}

	log.Println("Migrations applied successfully")
	return nil
}
