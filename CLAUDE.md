# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Go (module `telegram-business-bot`, Go 1.24+) Telegram business bot: a user bot, an optional separate admin bot, extra DB-configured "business bots", a USDT TRC20 wallet ledger, referrals, support tickets, message push campaigns, merchants, and a Gin web admin. MariaDB stores the data (GORM, AutoMigrate on startup) and Redis stores sessions and locks. User-facing text and docs are mostly Chinese.

## Commands

```bash
make test                      # go test ./...
go test ./internal/push -run TestName -v   # single test
make build                     # bin/xunzhou
make linux ARCH=arm64          # static cross-compile (CGO_ENABLED=0)
make dev                       # sources .env, then go run ./cmd/bot
docker compose up -d mariadb redis   # infra only, for local `make dev`
make up / make down / make logs      # full stack via Docker Compose
node --test internal/admin/ui_test.js   # admin UI JS tests (no browser needed)
TEST_MYSQL_DSN='user:pw@tcp(host:3306)/' go test ./integration/...  # real-DB financial tests
make test-integration                # same, in a throwaway MariaDB container
make e2e                             # whole app + fake Telegram + headless browser (e2e/run.sh)
```

The integration suite is skipped unless `TEST_MYSQL_DSN` is set. The DSN must **omit the database name**: the suite creates and drops its own uniquely named database and never reads `.env`.

## Architecture

- **Wiring**: `cmd/bot/main.go` → `internal/app/app.go` builds every service and starts the HTTP server (`/healthz`, `/telegram/webhook`, `/telegram/admin/webhook`, `/admin/*`, `/uploads`), the background workers (push, business bots, TRON watcher), and the Telegram loops (polling or webhook, set by `TELEGRAM_MODE`).
- **One `telegram.Router`, several bots**: the same router type serves the user bot, the admin bot (`adminOnly=true`; only `ADMIN_IDS`), and each business bot (`.WithBotID(id)`). Each one gets its own Redis session namespace (`sessions.WithNamespace(...)`). Business bots are created in `internal/business`: tokens are stored AES-GCM encrypted (key from `BUSINESS_BOT_KEY`, or the primary bot token if that is empty) and each bot is polled under a lease.
- **`internal/lease`**: a Redis SETNX lease with renewal. It serializes per-user update handling (`tg:lock:...`), per-campaign push work, and business-bot polling across replicas. If ownership is lost, the work's context is cancelled. Use it for any new work that must not run concurrently.
- **Update error semantics**: if `Router.Handle` returns an error, polling rewinds its offset and a webhook returns 503, so Telegram redelivers the update. Return errors only for failures where a retry is safe.
- **Wallet ledger** (`internal/wallet`): balances are split into available and frozen. Withdrawal flow: `available→frozen→pending→approved→completed` (completion deducts frozen; rejection returns frozen to available). Deposits from `trongrid.Watcher` are idempotent by `tx_id` and credit referral commission (`REFERRAL_RATE`) to the direct upline. The bot intentionally never holds TRON private keys or signs transactions; on-chain payout belongs in a separate signer.
- **Dynamic menus** (`internal/menu`): read from `menu_items` on every render (`builtin` / `url` / `submenu` action types), so menu changes need no restart. Default rows are seeded in `store.seedDefaults`.
- **Push** (`internal/push`): `push_campaigns` are expanded into `push_recipients` and sent with rate limiting and retries. Supports scheduled, recurring, pause/resume/cancel/retry, and per-category bot routing through `business.Resolve`.
- **Web admin** (`internal/admin`): Gin JSON API plus single-file `index.html` / `login.html` embedded via `go:embed`. All frontend JS is inline in `index.html`, and `ui_test.js` loads it into a `vm` sandbox. Accounts live in `admin_users` (roles `super`/`admin`; `.env` credentials only bootstrap the first super admin). A Redis session stores `id:sha256(password_hash)`, so a password change, disable or delete revokes it; use `operator(c)` for the acting account. Non-super accounts get page permissions from their `admin_roles` template (`permissionKeys` in `internal/admin/roles.go`); every API route in `Register` is wrapped with `can(<page keys>)`, so a new route must declare its page, and account/role routes use `requireSuper`.
- **H5 shop** (`internal/shop`): a Telegram Mini App served at `/shop/` (single file `web/index.html`, API under `/shop/api/*`) and opened by the menu button when `SHOP_H5_URL` is set. Requests carry `X-Telegram-Init-Data`, verified against the primary and every enabled business bot token (`business.Tokens`), and map to the same `telegram_users` row and wallet. Products belong to existing `merchants`; orders cover one merchant and one delivery type (express / pickup / virtual) and are paid on creation via `wallet.Spend` inside the order transaction (`wallet.Refund` on cancel). Stock is decremented with a conditional `UPDATE ... stock >= ?`; order creation is idempotent per `(user_id, request_id)` and a multi-merchant checkout (`CheckoutRequest.Groups`) creates every order in one transaction with request IDs `<id>#<n>`. After-sales (`aftersale.go`) refund partially into the wallet and track `refunded_amount`; `Service.Run` auto-completes shipped orders. Console accounts with role `merchant` are limited to their `merchant_id` via `merchantScope(c)` / `shop.Scope` on every shop admin route; `platformOnly` guards shared shop data. Promo prices are never stored as the current price: `effectivePrice` applies a SKU's `promo_price` only inside the product's promo window, and `lines()` / `cards()` / `Product()` all go through it. Completing an order calls `recordSale` (ledger entry keyed `sale:<order>`); refunds after completion call `recordRefund`; settlements lock and claim unsettled entries.
- **Shop v4**: `CreateOrders` locks the buyer's `telegram_users` row as its *first* statement (REPEATABLE READ: a plain read before the lock would fix a stale snapshot) so per-user limits (`limit_per_user`) hold; promo stock (`promo_stock`, nil = unlimited) is decremented conditionally and a sell-out is an error, never a silent price change. `Service.Run` also calls `GrantCommissions`, paying `shop_referral_rate` to the buyer's referrer once an order is completed and past `shop_aftersale_days` (`commission_done` makes it once-only). Share links are `/start p<productID>_<referralCode>` (parsed in the router); notifications and `/orders` use `telegram.ShopButton` web_app buttons.
- **Shop v5**: member points/tiers live in `member.go` (`shop_members`, `shop_points_logs`; tier and points discounts are computed after the coupon in `orderDiscounts`, and spending points locks the member row). Points and member spend are granted with referral commissions in `GrantCommissions`. Group buying (`group.go`): group orders start in status `grouping` (not fulfillable), `joinGroup` locks the `shop_groups` row, the order that fills the group flips all of them to `paid`, and `ExpireGroups` (in `Service.Run`, every 2 minutes) cancels and refunds the rest. Admin logins are throttled via `internal/ratelimit` (Redis fixed windows), H5 API calls per buyer too.
- **End-to-end checks**: `TELEGRAM_API_URL` lets the program talk to a fake Bot API server, so the whole app can run locally against temporary MariaDB/Redis and be driven with a headless browser (the H5 page needs `window.Telegram.WebApp.initData` signed with the bot token).
- **Shop v6**: buyer↔merchant messages (`message.go`, one `shop_conversations` row per buyer and merchant, unread counters on both sides) and merchant applications (`apply.go`; approval lives in `internal/admin/applications.go` because it creates the console account). `e2e/e2e.js` is the end-to-end script: extend it when adding user-facing flows; it fails on any page error, console error or HTTP ≥ 400. CI is `.github/workflows/ci.yml`.
- **Duplicate inserts**: do not use `OnConflict{DoNothing}` + `RowsAffected` to detect duplicates on tables with an auto-increment ID: on MariaDB GORM appends `RETURNING id`, so a skipped row still reports 1. Insert normally and check `store.IsDuplicate(err)`. Money-path tests live in `integration/shop_test.go` and need `TEST_MYSQL_DSN`.
- **Schema changes**: add the model in `internal/model/models.go` and register it in the `AutoMigrate` list in `internal/store/store.go`.

## Testing conventions

Service unit tests use `internal/testutil.DB(t, steps...)`, a scripted fake SQL driver. Each `Step` must match the next query in order (`Contains` substring, optional `Args`), and any unexecuted steps fail the test. When you change a service's queries, update the step scripts to match. Redis-dependent code takes small interfaces (e.g. `lease.Client`) so tests can fake it.

`internal/app/app_media_integration.go.txt` is a patch note, not compiled code.
