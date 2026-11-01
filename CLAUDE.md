# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o finbot .
./finbot serve                 # daemon: delta sync now + every SYNC_INTERVAL, /healthz on HTTP_ADDR
./finbot sync                  # one delta sync of the active plan, print a one-line summary, exit
./finbot sync --archived       # persisted scan of every plan for Discretionary, then SyncPlan each one that has it
./finbot health                # GET /healthz on the local server (HTTP_ADDR); exit 0 on 200, else 1
./finbot backup                # store one encrypted backup off-site now
./finbot backup --list         # list stored backups (id, time, size)
./finbot restore --key <path> [--id <id>] --out <path>   # decrypt a backup (newest by default) to a new file, verify it
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

Docker and deploy:
```bash
docker build --build-arg GIT_SHA=$(git rev-parse --short HEAD) -t finbot .
docker run --env-file .env -v finbot-data:/data finbot serve
./deploy/deploy.sh                     # deploy to hoid, from the repo root, then wait for healthy
```
The image runs as uid 10001. The `GIT_SHA` build arg sets `cmd.Version` (default `dev`), shown in the Slack status
message. `deploy.sh` ships `git archive HEAD` (it refuses uncommitted tracked changes; `spikes`, `docs` and env files
stay out) to `hoid:~/finbot/src` and writes `~/finbot/finbot.env` (0600) from the finbot keys in the local `.env`. It
runs compose project `finbot` with `deploy/compose.yaml`, which sets `TZ=America/Denver` and `IMPORT_ENABLED=false`.

## Architecture

finbot mirrors YNAB plans into one local SQLite file and keeps the active plan current with one delta request per tick.
The old 6-hour LLM/vector categorizer (and its Turso, Vertex and OpenAI stack) is gone; categorization returns as
`classify/` in M3.

**Package roles:**
- `main.go` → `cmd.Main`: subcommand dispatch (stdlib `flag`).
- `cmd/` — the subcommands and the scheduler. `Scheduler` runs the sync job immediately, then every `SYNC_INTERVAL`; the
  import job every `IMPORT_INTERVAL` only when `IMPORT_ENABLED`; and the backup job nightly at `BACKUP_AT` when backups
  are configured. The Slack status update runs after each scheduled sync and backup, as part of
  that job (`finbot sync` and `finbot backup` never touch it). All jobs run on one goroutine, so ticks never overlap; a job's own tick that comes due while it runs is dropped; other jobs' ticks run
  right after. Each scheduled sync also sends the optional dead-man ping (`HEALTHCHECK_PING_URL`). `Health` serves `GET /healthz`: 200
  with `{"last_sync_ok","last_error"}` while a sync succeeded within 2×`SYNC_INTERVAL`, else 503 (a rate-budget skip
  counts as not fresh). Logs are `log/slog` JSON; `sync`/`ynab`'s `*log.Logger` is adapted with `slog.NewLogLogger`.
  Graceful shutdown on SIGINT/SIGTERM cancels the running tick, closes the HTTP server and the DB.
- `config/` — env loading (godotenv for `.env`). `LoadFrom(getenv)` for tests. Errors name the variable, never the value.
- `store/` — SQLite via `modernc.org/sqlite` (pure Go, CGO off), WAL, separate writer/reader pools, embedded
  migrations in `store/migrations/` (`0001_mirror`, `0002_plan_scan`, `0003_status_message`). Mirror tables per
  plan, `sync_state` cursor, `plan_scan` (persisted archived scan; read with `DB.PlanScans`), `status_message` (the Slack message and last
  backup outcome, kept across restarts). Timestamps are fixed-width UTC strings.
- `sync/` — `Syncer.SyncPlan`: full export the first time a plan is seen, then one delta per tick; rows and cursor
  commit in one transaction. Stitches subtransactions, emits bank-link `LinkEvent`s on transitions, counts orphans.
  `ScanDiscretionary` scans every plan (archived included) and persists the result. `TriggerImport` is the bank-import
  POST. Nothing logged carries an id.
- `ynab/` — thin wrapper over `pkg.venceslau.dev/ynab` (V4) with a zone-normalizing, body-capping HTTP transport
  (`goal_snoozed_at`). A local rate governor (200/hr, write reserve) runs as the library's `Limiter`; write intent
  travels on ctx (`asWrite`). Every error has the token, plan id and other ids redacted. `ImportTransactions` is the
  only YNAB write.
- `backup/` — age-encrypted `VACUUM INTO` snapshots stored as 1 MiB chunks in Turso, which only ever sees ciphertext;
  keeps `BACKUP_KEEP`. Refuses a snapshot that fails `integrity_check` or has no mirror rows at all. A run is bounded by
  10 min. `Restore` uses the private key (on the Mac only), never overwrites `--out`, and verifies before it appears.
- `status/` — keeps one Slack message current, edited after every scheduled sync and backup; it's a heartbeat, and Brendan's own
  Slack automation alerts when it goes stale. Slack failures are only logged and never fail the job. Each update is
  capped at 20 s, plus a 5 s store write that outlives the run's ctx. A new message is posted at most once per 6h.

**Key data flow:** `finbot serve` → `Scheduler` tick → `Syncer.SyncPlan(activePlan)` → `ynab.Client.Export`/`Delta` →
`store` upsert + cursor in one tx → slog line (initial/delta, skipped, row counts, orphans, link events by account name)
→ ping and Slack status edit, if configured → `Health.OK`/`Fail`.

## Safety

- YNAB access is read-only except `ImportTransactions`. `IMPORT_ENABLED` defaults to false; turning it on against the
  live plan needs Brendan's OK.
- Never log or print ids, the token or the plan id; plan and account names are fine. `HEALTHCHECK_PING_URL`,
  `BACKUP_TURSO_URL`, `BACKUP_TURSO_TOKEN` and `SLACK_BOT_TOKEN` are secrets too.
- `./finbot serve` or `docker run --env-file .env` with the real `.env` acts on real services: a fresh DB posts a new
  status message to the real channel, and a backup that comes due goes to the real `finbot-backup` and prunes it. Blank
  `SLACK_BOT_TOKEN` and `BACKUP_*` for local runs.

## Testing

External dependencies sit behind interfaces with fakes (`sync.PlanAPI`, `sync.PlanLister`, an injectable `Ticker` and
clock in `cmd`). Store tests use a temp SQLite file. Live tests are env-gated (above).

## Configuration

All config is via environment variables (loaded from `.env`); see `.env.example`. Two exceptions: `finbot health` reads
`HTTP_ADDR` from the real environment only, and `deploy.sh` forwards only `YNAB_ACCESS_TOKEN`, `YNAB_BUDGET_ID`,
`HEALTHCHECK_PING_URL`, `BACKUP_*` and `SLACK_*`, so other keys in `.env` (such as `SYNC_INTERVAL`, `IMPORT_INTERVAL`,
`LOG_LEVEL`, `HTTP_ADDR`) never reach hoid.
Required: `YNAB_ACCESS_TOKEN`, `YNAB_BUDGET_ID` (the active plan). Defaults: `FINBOT_DB_PATH=/data/finbot.db`,
`SYNC_INTERVAL=15m`, `IMPORT_INTERVAL=6h`, `IMPORT_ENABLED=false`, `HTTP_ADDR=:8080`, `LOG_LEVEL=info`.

Optional:
- `HEALTHCHECK_PING_URL`: dead-man ping after each scheduled sync. Empty disables it.
- `BACKUP_AGE_RECIPIENT`, `BACKUP_TURSO_URL`, `BACKUP_TURSO_TOKEN`: backups are disabled unless all three are set.
- `BACKUP_AT=03:17`: local time of day, so Denver time on hoid. `BACKUP_KEEP=14`.
- `SLACK_BOT_TOKEN`: empty disables the status message. `SLACK_STATUS_CHANNEL=finbot-finances`: the bot must be a member.
