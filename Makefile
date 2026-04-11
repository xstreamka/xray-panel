.PHONY: up down build logs restart

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
