.PHONY: up down build logs restart xray-logs xray-access

up:
	docker compose up -d --build

down:
	docker compose down

build:
	docker compose build --no-cache

logs:
	docker compose logs -f panel

restart:
	docker compose restart panel

# Локальная разработка (без Docker)
run:
	go run ./cmd/server

# Генерация ключей Reality
xray-keys:
	docker run --rm teddysun/xray xray x25519

# Логи Xray
xray-logs:
	docker compose logs -f xray

xray-access:
	docker compose exec panel tail -f /var/log/xray/access.log

xray-error:
	docker compose exec panel tail -f /var/log/xray/error.log

# Проверить сгенерированный конфиг
xray-config:
	docker compose exec panel cat /etc/xray/config.json