GO ?= go
ARCH ?= amd64

.PHONY: up down logs dev build tidy test test-integration e2e linux linux-amd64 linux-arm64 linux-all
up:
	docker compose up -d --build
down:
	docker compose down
logs:
	docker compose logs -f bot
tidy:
	$(GO) mod tidy
dev:
	set -a; . ./.env; set +a; $(GO) run ./cmd/bot
build:
	$(GO) build -o bin/xunzhou ./cmd/bot
test:
	$(GO) test ./...

# Real-database tests in a throwaway MariaDB container.
test-integration:
	docker run -d --rm --name tg-it-db -e MARIADB_ROOT_PASSWORD=it -p 127.0.0.1:33198:3306 mariadb:11.4 >/dev/null
	until docker exec tg-it-db mariadb -uroot -pit -e "select 1" >/dev/null 2>&1; do sleep 1; done
	TEST_MYSQL_DSN='root:it@tcp(127.0.0.1:33198)/' $(GO) test -count=1 ./integration/... ; status=$$?; docker stop tg-it-db >/dev/null; exit $$status

# Whole app in a headless browser against a fake Telegram Bot API (see e2e/run.sh).
e2e:
	./e2e/run.sh

# Cross-compile without a C toolchain; filenames include the target architecture.
linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags="-s -w" -o bin/xunzhou-$(ARCH) ./cmd/bot

linux-amd64:
	$(MAKE) linux ARCH=amd64

linux-arm64:
	$(MAKE) linux ARCH=arm64

linux-all: linux-amd64 linux-arm64
