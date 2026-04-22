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

	`CREATE INDEX IF NOT EXISTS idx_users_verify_token ON users(verify_token) WHERE verify_token IS NOT NULL`,

	// Таблица тарифов — управляется через /admin/tariffs
	`CREATE TABLE IF NOT EXISTS tariffs (
		id          SERIAL PRIMARY KEY,
		code        VARCHAR(50) UNIQUE NOT NULL,       -- "basic_10" — уходит в pay-service как plan_id
		label       VARCHAR(100) NOT NULL,             -- UI-название "10 ГБ"
		description VARCHAR(255) NOT NULL DEFAULT '',  -- в чек Робокассы, только ASCII
		amount_rub  NUMERIC(10,2) NOT NULL,
		traffic_gb  NUMERIC(10,2) NOT NULL,
		is_popular  BOOLEAN DEFAULT FALSE,
		is_active   BOOLEAN DEFAULT TRUE,
		sort_order  INTEGER DEFAULT 0,
		created_at  TIMESTAMPTZ DEFAULT NOW(),
		updated_at  TIMESTAMPTZ DEFAULT NOW()
	)`,

	`CREATE INDEX IF NOT EXISTS idx_tariffs_active ON tariffs(is_active)`,
	`CREATE INDEX IF NOT EXISTS idx_tariffs_sort ON tariffs(sort_order)`,

	// Сид стартовых тарифов — только если таблица полностью пустая.
	// Если админ удалил все тарифы и перезапустил — восстановится; если один из трёх
	// удалён, seed не сработает, и обратно они не вернутся.
	`INSERT INTO tariffs (code, label, description, amount_rub, traffic_gb, is_popular, sort_order)
	 SELECT * FROM (VALUES
		('basic_10', '10 ГБ',  'VPN Panel 10 GB',  150::numeric, 10::numeric,  FALSE, 10),
		('plus_30',  '30 ГБ',  'VPN Panel 30 GB',  300::numeric, 30::numeric,  TRUE,  20),
		('pro_100',  '100 ГБ', 'VPN Panel 100 GB', 700::numeric, 100::numeric, FALSE, 30)
	 ) AS t(code, label, description, amount_rub, traffic_gb, is_popular, sort_order)
	 WHERE NOT EXISTS (SELECT 1 FROM tariffs)`,

	// Квитанции об оплате — идемпотентность webhook + аудит пополнений
	`CREATE TABLE IF NOT EXISTS payment_receipts (
		id             SERIAL PRIMARY KEY,
		inv_id         INTEGER UNIQUE NOT NULL,             -- из pay-service
		user_id        INTEGER REFERENCES users(id) ON DELETE SET NULL,
		plan_id        VARCHAR(50) NOT NULL,                -- tariffs.code на момент оплаты
		amount_rub     NUMERIC(10,2) NOT NULL,
		traffic_bytes  BIGINT NOT NULL,                     -- сколько начислено
		paid_at        TIMESTAMPTZ NOT NULL,
		raw_payload    JSONB NOT NULL,                      -- сырой JSON от pay-service
		created_at     TIMESTAMPTZ DEFAULT NOW()
	)`,

	`CREATE INDEX IF NOT EXISTS idx_payment_receipts_user_id ON payment_receipts(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_payment_receipts_created_at ON payment_receipts(created_at DESC)`,

	// ============================================
	// Подписочная модель (subscription + addons)
	// ============================================

	// tariffs: добавляем тип и срок действия
	`ALTER TABLE tariffs ADD COLUMN IF NOT EXISTS duration_days INT NOT NULL DEFAULT 30`,
	`ALTER TABLE tariffs ADD COLUMN IF NOT EXISTS kind VARCHAR(20) NOT NULL DEFAULT 'subscription'`,

	`DO $$ BEGIN
    ALTER TABLE tariffs ADD CONSTRAINT tariffs_kind_check
        CHECK (kind IN ('subscription', 'addon'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$`,

	`DO $$ BEGIN
    ALTER TABLE tariffs ADD CONSTRAINT tariffs_duration_valid
        CHECK (
            (kind = 'subscription' AND duration_days > 0) OR
            (kind = 'addon')
        );
EXCEPTION WHEN duplicate_object THEN NULL;
END $$`,

	// users: новые колонки (каждая отдельным statement, никаких DO-блоков)
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS current_tariff_id INTEGER REFERENCES tariffs(id) ON DELETE SET NULL`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS tariff_expires_at TIMESTAMPTZ`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS base_traffic_limit BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS base_traffic_used BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS extra_traffic_balance BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS frozen_extra_balance BIGINT NOT NULL DEFAULT 0`,
	// extra_traffic_granted — исходный объём extra в текущем цикле подписки.
	// Нужен, чтобы считать «потрачено из extra» для прогресс-бара.
	// += при каждом пополнении (addon-платёж, админский set), сбрасывается при
	// продлении подписки (→ размороженный frozen), обнуляется при истечении.
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS extra_traffic_granted BIGINT NOT NULL DEFAULT 0`,
	// На существующих юзерах синхронизируем granted = текущий balance
	// (считаем что весь имеющийся extra «свежевыдан»).
	`UPDATE users SET extra_traffic_granted = extra_traffic_balance
	 WHERE extra_traffic_granted = 0 AND extra_traffic_balance > 0`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS reminder_5d_sent_at TIMESTAMPTZ`,
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS reminder_1d_sent_at TIMESTAMPTZ`,
	// Метка «юзеру уже отправили письмо об отключении VPN» (balance/expired).
	// Ставится в момент отправки, сбрасывается при любом пополнении
	// (AddExtra/SetExtra/RenewSubscription/ApplyPayment), чтобы не спамить.
	`ALTER TABLE users ADD COLUMN IF NOT EXISTS block_notified_at TIMESTAMPTZ`,

	// Снос legacy-колонки: данные уже перенесены в extra_traffic_balance
	// более ранней миграцией (на существующих БД). На чистых БД — no-op.
	`ALTER TABLE users DROP COLUMN IF EXISTS traffic_balance`,

	// Индекс для крона обработки истёкших подписок
	`CREATE INDEX IF NOT EXISTS idx_users_tariff_expires
    ON users(tariff_expires_at)
    WHERE tariff_expires_at IS NOT NULL`,

	// payment_receipts: денормализованное поле для фильтрации по виду тарифа
	`ALTER TABLE payment_receipts ADD COLUMN IF NOT EXISTS tariff_kind VARCHAR(20)`,

	// Сид одного addon-тарифа, если в таблице ещё нет ни одного аддона.
	// Базовые подписочные тарифы засеваются выше с дефолтным kind='subscription'.
	`INSERT INTO tariffs (code, label, description, amount_rub, traffic_gb, duration_days, kind, sort_order)
	 SELECT 'topup_20', '+20 ГБ', 'VPN Panel addon 20 GB', 200::numeric, 20::numeric, 0, 'addon', 100
	 WHERE NOT EXISTS (SELECT 1 FROM tariffs WHERE kind = 'addon')`,
}

func (db *DB) Migrate() error {
	ctx := context.Background()

	for i, m := range migrations {
		if _, err := db.Pool.Exec(ctx, m); err != nil {
			// Снимем первую строку SQL для удобства в логах
			firstLine := m
			if idx := indexByte(firstLine, '\n'); idx > 0 {
				firstLine = firstLine[:idx]
			}
			return fmt.Errorf("migration %d failed: %w\nSQL: %s", i, err, firstLine)
		}
	}

	log.Println("Migrations applied successfully")
	return nil
}

// Маленький helper, если не хочется тянуть strings ради одной функции
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
