#!/usr/bin/env bash
# End-to-end check: runs the real program against temporary MariaDB and Redis containers and a
# fake Telegram Bot API, then walks the admin console, the H5 shop and the bot in a headless
# browser (see e2e.js). Usage: ./e2e/run.sh   (set CHROME=/path/to/chrome to use a local browser)
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd .. && pwd)
WORK="$PWD/.work"
APP_PORT=${E2E_APP_PORT:-18731}
TG_PORT=${E2E_TG_PORT:-38081}
DB_PORT=${E2E_DB_PORT:-33199}
REDIS_PORT=${E2E_REDIS_PORT:-36399}
export E2E_DB_CONTAINER=${E2E_DB_CONTAINER:-e2e-db}
REDIS_CONTAINER=e2e-redis

mkdir -p "$WORK"
cleanup() {
  [[ -n "${APP_PID:-}" ]] && kill "$APP_PID" 2>/dev/null || true
  [[ -n "${TG_PID:-}" ]] && kill "$TG_PID" 2>/dev/null || true
  docker stop "$E2E_DB_CONTAINER" "$REDIS_CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker run -d --rm --name "$E2E_DB_CONTAINER" -e MARIADB_ROOT_PASSWORD=e2e -e MARIADB_DATABASE=e2e -p "127.0.0.1:$DB_PORT:3306" mariadb:11.4 >/dev/null
docker run -d --rm --name "$REDIS_CONTAINER" -p "127.0.0.1:$REDIS_PORT:6379" redis:7.4-alpine >/dev/null
(cd "$ROOT" && go build -o "$WORK/bot" ./cmd/bot)
for i in $(seq 1 60); do docker exec "$E2E_DB_CONTAINER" mariadb -uroot -pe2e -e "select 1" e2e >/dev/null 2>&1 && break; sleep 1; done

python3 fake_telegram.py "$TG_PORT" > "$WORK/fake_telegram.log" 2>&1 &
TG_PID=$!
(
  cd "$WORK"
  export APP_ENV=prod HTTP_ADDR="127.0.0.1:$APP_PORT" TELEGRAM_BOT_TOKEN=111:primary TELEGRAM_MODE=polling ADMIN_IDS=777 \
    ADMIN_WEB_USER=admin ADMIN_WEB_PASSWORD=e2e-password-123 \
    MYSQL_DSN="root:e2e@tcp(127.0.0.1:$DB_PORT)/e2e?charset=utf8mb4&parseTime=True&loc=Local" \
    REDIS_ADDR="127.0.0.1:$REDIS_PORT" REDIS_DB=0 SESSION_TTL_MINUTES=30 WALLET_CURRENCY=USDT WITHDRAW_MIN_USDT=1 WITHDRAW_FEE_USDT=1 \
    REFERRAL_RATE=0.005 TRONGRID_BASE_URL=https://api.shasta.trongrid.io TRON_CONFIRM_ONLY=true TRON_WATCH_ENABLED=false \
    TRON_WATCH_INTERVAL_SECONDS=30 UPLOAD_DIR=./uploads UPLOAD_PUBLIC_BASE_URL=https://e2e.example/uploads UPLOAD_MAX_IMAGE_MB=10 \
    UPLOAD_MAX_VIDEO_MB=50 TELEGRAM_API_URL="http://127.0.0.1:$TG_PORT" SHOP_H5_URL=https://e2e.example/shop/ \
    ADMIN_PUBLIC_URL=https://e2e.example/admin/ LOG_DIR=./logs
  exec ./bot
) > "$WORK/app.log" 2>&1 &
APP_PID=$!
for i in $(seq 1 60); do curl -sf "http://127.0.0.1:$APP_PORT/healthz" >/dev/null && break; sleep 1; done
curl -sf "http://127.0.0.1:$APP_PORT/healthz" >/dev/null || { echo "app did not start"; tail -30 "$WORK/app.log"; exit 1; }

[[ -d node_modules ]] || npm ci --silent
E2E_BASE="http://127.0.0.1:$APP_PORT" E2E_TG="http://127.0.0.1:$TG_PORT" E2E_SHOTS="$WORK/shots" node e2e.js
