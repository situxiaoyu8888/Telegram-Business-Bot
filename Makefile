GO ?= go
ARCH ?= amd64

.PHONY: up down logs dev build tidy test linux linux-amd64 linux-arm64 linux-all
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

# Cross-compile without a C toolchain; filenames include the target architecture.
linux:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags="-s -w" -o bin/xunzhou-$(ARCH) ./cmd/bot

linux-amd64:
	$(MAKE) linux ARCH=amd64

linux-arm64:
	$(MAKE) linux ARCH=arm64

linux-all: linux-amd64 linux-arm64
