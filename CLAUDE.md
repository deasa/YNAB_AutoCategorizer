# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o finbot .
./finbot serve                 # daemon: delta sync now + every SYNC_INTERVAL, /healthz on HTTP_ADDR
./finbot sync                  # one delta sync of the active plan, print a one-line summary, exit
./finbot sync --archived       # persisted scan of every plan for Discretionary, then SyncPlan each one that has it
./finbot                       # usage, exit 2 (also for unknown subcommands)

go test ./...                          # all tests
go test -v ./sync                      # single package
go test -run TestScheduler_ ./cmd      # single test pattern
```

Env-gated live tests (read-only against the real plans, need `.env`):
```bash
YNAB_LIVE_SMOKE=1 go test ./ynab -run TestLiveSmoke -v       # two GETs
YNAB_LIVE_SYNC=1 go test ./sync -run TestLiveSync -v         # initial + delta on the active plan
YNAB_LIVE_SYNC=1 go test ./sync -run 'TestLiveScan$' -v      # archived-plan scan over every plan
```
Set `YNAB_LIVE_SYNC_LOG=<path>` to keep the live sync log. Grep any live output for UUIDs, the token and the plan id
before showing it.

Docker:
```bash
docker build -t finbot .
docker run --env-file .env -v finbot-data:/data finbot serve
```

## Architecture

finbot mirrors YNAB plans into one local SQLite file and keeps the active plan current with one delta request per tick.
The old 6-hour LLM/vector categorizer (and its Turso, Vertex and OpenAI stack) is gone; categorization returns as
`classify/` in M3.

**Package roles:**
- `main.go` → `cmd.Main`: subcommand dispatch (stdlib `flag`).
- `cmd/` — `serve` and `sync`. `Scheduler` runs the sync job immediately, then every `SYNC_INTERVAL`, and the import
  job every `IMPORT_INTERVAL` only when `IMPORT_ENABLED`. All jobs run on one goroutine, so ticks never overlap; ticks
  that come due during a job are dropped. `Health` serves `GET /healthz`: 200 with
  `{"last_sync_ok","last_error"}` while a sync succeeded within 2×`SYNC_INTERVAL`, else 503 (a rate-budget skip
  counts as not fresh). Logs are `log/slog` JSON; `sync`/`ynab`'s `*log.Logger` is adapted with `slog.NewLogLogger`.
  Graceful shutdown on SIGINT/SIGTERM cancels the running tick, closes the HTTP server and the DB.
- `config/` — env loading (godotenv for `.env`). `LoadFrom(getenv)` for tests. Errors name the variable, never the value.
- `store/` — SQLite via `modernc.org/sqlite` (pure Go, CGO off), WAL, separate writer/reader pools, embedded
  migrations in `store/migrations/` (`0001_mirror`, `0002_plan_scan`). Mirror tables per plan, `sync_state` cursor,
  `plan_scan` (persisted archived scan; read with `DB.PlanScans`). Timestamps are fixed-width UTC strings.
- `sync/` — `Syncer.SyncPlan`: full export the first time a plan is seen, then one delta per tick; rows and cursor
  commit in one transaction. Stitches subtransactions, emits bank-link `LinkEvent`s on transitions, counts orphans.
  `ScanDiscretionary` scans every plan (archived included) and persists the result. `TriggerImport` is the bank-import
  POST. Nothing logged carries an id.
- `ynab/` — thin wrapper over `pkg.venceslau.dev/ynab` (V4) with a zone-normalizing, body-capping HTTP transport
  (`goal_snoozed_at`). A local rate governor (200/hr, write reserve) runs as the library's `Limiter`; write intent
  travels on ctx (`asWrite`). Every error has the token, plan id and other ids redacted. `ImportTransactions` is the
  only YNAB write.

**Key data flow:** `finbot serve` → `Scheduler` tick → `Syncer.SyncPlan(activePlan)` → `ynab.Client.Export`/`Delta` →
`store` upsert + cursor in one tx → slog line (initial/delta, skipped, row counts, orphans, link events by account name)
→ `Health.OK`/`Fail`.

## Safety

- YNAB access is read-only except `ImportTransactions`. `IMPORT_ENABLED` defaults to false; turning it on against the
  live plan needs Brendan's OK.
- Never log or print ids, the token or the plan id; plan and account names are fine.

## Testing

External dependencies sit behind interfaces with fakes (`sync.PlanAPI`, `sync.PlanLister`, an injectable `Ticker` and
clock in `cmd`). Store tests use a temp SQLite file. Live tests are env-gated (above).

## Configuration

All config is via environment variables (loaded from `.env`); see `.env.example`.
Required: `YNAB_ACCESS_TOKEN`, `YNAB_BUDGET_ID` (the active plan). Defaults: `FINBOT_DB_PATH=/data/finbot.db`,
`SYNC_INTERVAL=15m`, `IMPORT_INTERVAL=6h`, `IMPORT_ENABLED=false`, `HTTP_ADDR=:8080`, `LOG_LEVEL=info`.
