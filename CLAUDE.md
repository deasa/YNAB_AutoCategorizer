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
./finbot revoke <email | u_hash | --all>         # sign a user (or everyone) out of MCP at once, no restart; needs only FINBOT_DB_PATH
./finbot tag-account [<account name> <bucket>]   # set an account's tax bucket, or list every tag; needs only FINBOT_DB_PATH, YNAB_BUDGET_ID
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
./deploy/cutover.sh                    # move the public hostname from the finbot-spike connector to finbot's; --watch re-attaches
./deploy/rollback.sh                   # back to the spike's connector; safe to re-run
./deploy/rehearse.sh                   # deploy, cutover and rollback against a fake hoid; never reads .env
./deploy/rehearse-docker.sh            # real image, compose file, listeners and cloudflared flags, dummy credentials
```
The image runs as uid 10001. The `GIT_SHA` build arg sets `cmd.Version` (default `dev`), shown in the Slack status
message. `deploy.sh` ships `git archive HEAD` (it refuses uncommitted tracked changes; `spikes`, `docs` and env files
stay out) to `hoid:~/finbot/src` and writes `~/finbot/finbot.env` (0600) from the finbot keys in the local `.env`. It
runs compose project `finbot` with `deploy/compose.yaml`, which sets `TZ=America/Denver`, `IMPORT_ENABLED=true` and
`HTTP_ADDR=127.0.0.1:8081`. `deploy.sh` refuses an `.env` with no value for the Google or allowlist keys.

`cloudflared` is compose profile `tunnel`: `deploy.sh` enables it only when the marker `~/finbot/tunnel-active` exists,
and then first checks `~/finbot/tunnel.env` (0600, exactly one `TUNNEL_TOKEN=` line). `cutover.sh` creates the marker
and `rollback.sh` removes it. Cutover steps 3 to 6 and their rollback run detached on hoid (`cutover-remote.sh`, with
`rollback-remote.sh` holding the rollback), logging to `~/finbot/cutover.log` (0600), which ends in one `RESULT` line.
`cutover.sh` exits 0 cut over, 1 failed and rolled back or stopped before step 3, 2 rollback failed, 3 outcome unknown.

## Architecture

finbot mirrors YNAB plans into one local SQLite file and keeps the active plan current with one delta request per tick.
The old 6-hour LLM/vector categorizer (and its Turso, Vertex and OpenAI stack) is gone; categorization returns as
`classify/` in M3.

**Package roles:**
- `main.go` → `cmd.Main`: subcommand dispatch (stdlib `flag`).
- `cmd/` — the subcommands and the scheduler. `Scheduler` runs the sync job immediately, then every `SYNC_INTERVAL`; the
  import job every `IMPORT_INTERVAL` only when `IMPORT_ENABLED`; and the backup job nightly at `BACKUP_AT` when backups
  are configured. When MCP is on, a prune job deletes expired OAuth rows at startup and every 24h; its outcome never
  touches `Health`. The Slack status update runs after each scheduled sync and backup, as part of that job (`finbot sync`
  and `finbot backup` never touch it). All jobs run on one goroutine, so ticks never overlap; a job's own tick that
  comes due while it runs is dropped; other jobs' ticks run right after. Each scheduled sync also sends the optional
  dead-man ping (`HEALTHCHECK_PING_URL`). `Health` serves `GET /healthz` on its own private listener (`HTTP_ADDR`), since
  it returns `last_error` and the tunnel forwards all of the MCP port: 200 with `{"last_sync_ok","last_error"}` while
  a sync succeeded within 2×`SYNC_INTERVAL`, else 503 (a rate-budget skip counts as not fresh). Logs are `log/slog`
  JSON; `sync`/`ynab`'s `*log.Logger` is adapted with `slog.NewLogLogger`. Graceful shutdown on SIGINT/SIGTERM cancels
  the running tick, closes the HTTP servers and the DB.
- `config/` — env loading (godotenv for `.env`). `LoadFrom(getenv)` for tests. Errors name the variable, never the value.
- `store/` — SQLite via `modernc.org/sqlite` (pure Go, CGO off), WAL, separate writer/reader pools, embedded migrations
  in `store/migrations/` (`0001_mirror`, `0002_plan_scan`, `0003_status_message`, `0004_plan_scan_keep_verdict`,
  `0005_oauth`). Database files are kept 0600. Mirror tables per plan, `sync_state` cursor, `plan_scan` (persisted
  archived scan; read with `DB.PlanScans`), `status_message` (the Slack message and last backup outcome, kept across
  restarts). Timestamps are fixed-width UTC strings. Read queries for MCP: split-aware `QueryLines` and `CategoryTotals`
  in `store/transactions_read.go`; `Accounts`, `MonthlyFlows`, `HistoryRange`, `Goals` and `IncomeLines` in
  `store/snapshot_read.go`. Income excludes transfers in from off-budget accounts that YNAB categorizes to Ready to
  Assign (moved savings, not income).
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
  Snapshots also hold the OAuth state (HMAC secret and token signatures), so run `finbot revoke --all` after a restore.
- `auth/` — the OAuth 2.1 authorization server for MCP: `ory/fosite`, sign-in delegated to Google through `go-oidc`,
  metadata types from the MCP go-sdk. Routes on the MCP listener: `/.well-known/*` and `/oauth/` (`register`, `authorize`,
  `google/callback`, `consent`, `token`, `revoke`). Only verified emails in `MCP_ALLOWED_EMAILS` get in; a user is the
  subject `u_<first 12 hex of sha256(email)>`, and raw emails are never logged. State persists in SQLite, keyed by
  HMAC signature (raw tokens are never stored). Access tokens last 1h, refresh tokens 30 days. `token_generations` backs
  `finbot revoke`: tokens check it on every request, so a running server needs no restart. `ActiveTokens` counts what a
  revoke would cut off.
- `mcp/` — Streamable HTTP resource server at `/mcp` (go-sdk), behind bearer auth, on `MCP_LISTEN_ADDR`. Disabled unless
  `MCP_PUBLIC_URL`, both `GOOGLE_OAUTH_*` values and `MCP_ALLOWED_EMAILS` are set. The routes answer 503 until Google
  discovery succeeds (retried in the background; the mirror keeps syncing). Tools read SQLite only and never call YNAB:
  `data_freshness`, `query_transactions`, `compare_spending`, `get_financial_snapshot`, `get_goals`, `get_income_events`.
  Every response carries data freshness: `last_synced_at`, `age_seconds`, `stale` (older than 2×`SYNC_INTERVAL` or never
  synced), `links_ok`, `accounts`, `warnings`; a linked account with no import for over 14 days is flagged
  `no_recent_import`. `badRequest` is the only way to send a client a specific error, built from the caller's input and
  fixed text; everything else reaches the client as a generic failure. Snapshot averages are day-precise when history
  starts mid-window (total × 30.4375 / days), so a partial first month doesn't deflate them.
- `status/` — keeps one Slack message current, edited after every scheduled sync and backup; it's a heartbeat, and
  Brendan's own Slack automation alerts when it goes stale. Slack failures are only logged and never fail the job. Each
  update is capped at 20 s, plus a 5 s store write that outlives the run's ctx. A new message is posted at most once per
  6h.

**Key data flow:** `finbot serve` → `Scheduler` tick → `Syncer.SyncPlan(activePlan)` → `ynab.Client.Export`/`Delta` →
`store` upsert + cursor in one tx → slog line (initial/delta, skipped, row counts, orphans, link events by account name)
→ ping and Slack status edit, if configured → `Health.OK`/`Fail`.

## Safety

- YNAB access is read-only except `ImportTransactions`. The `IMPORT_ENABLED` config default stays false; the hoid deploy
  pins it true in `deploy/compose.yaml` (Brendan approved, BA-13, 2026-11-08), so that deploy POSTs to the live plan
  every `IMPORT_INTERVAL`. Turning it on anywhere else against the live plan still needs Brendan's OK.
- Never log or print ids, the token or the plan id; plan and account names are fine. `HEALTHCHECK_PING_URL`,
  `BACKUP_TURSO_URL`, `BACKUP_TURSO_TOKEN`, `SLACK_BOT_TOKEN`, `GOOGLE_OAUTH_CLIENT_SECRET` and `TUNNEL_TOKEN` are secrets
  too, and allowlisted emails are never logged.
- `./finbot serve` or `docker run --env-file .env` with the real `.env` acts on real services: a fresh DB posts a new
  status message to the real channel, and a backup that comes due goes to the real `finbot-backup` and prunes it. Blank
  `SLACK_BOT_TOKEN` and `BACKUP_*` for local runs.

## Testing

External dependencies sit behind interfaces with fakes (`sync.PlanAPI`, `sync.PlanLister`, an injectable `Ticker` and
clock in `cmd`). Store tests use a temp SQLite file. Live tests are env-gated (above).

## Configuration

All config is via environment variables (loaded from `.env`); see `.env.example`. Two exceptions: `finbot health` reads
`HTTP_ADDR` from the real environment only, and `deploy.sh` forwards only `YNAB_ACCESS_TOKEN`, `YNAB_BUDGET_ID`,
`HEALTHCHECK_PING_URL`, `BACKUP_*`, `SLACK_*`, `GOOGLE_OAUTH_*` and `MCP_ALLOWED_EMAILS`, so other keys in `.env` (such as
`SYNC_INTERVAL`, `IMPORT_INTERVAL`, `LOG_LEVEL`, `HTTP_ADDR`, `MCP_PUBLIC_URL`) never reach hoid; `deploy/compose.yaml`
sets the hoid values of `HTTP_ADDR`, `MCP_LISTEN_ADDR` and `MCP_PUBLIC_URL`.
Required: `YNAB_ACCESS_TOKEN`, `YNAB_BUDGET_ID` (the active plan). Defaults: `FINBOT_DB_PATH=/data/finbot.db`,
`SYNC_INTERVAL=15m`, `IMPORT_INTERVAL=6h`, `IMPORT_ENABLED=false`, `HTTP_ADDR=127.0.0.1:8081`, `MCP_LISTEN_ADDR=:8080`,
`LOG_LEVEL=info`. A local `.env` copied from an old example may still have `HTTP_ADDR=:8080`: remove that line, or with
MCP on `serve` fails at startup with "address already in use".

Optional:
- `HEALTHCHECK_PING_URL`: dead-man ping after each scheduled sync. Empty disables it.
- `BACKUP_AGE_RECIPIENT`, `BACKUP_TURSO_URL`, `BACKUP_TURSO_TOKEN`: backups are disabled unless all three are set.
- `BACKUP_AT=03:17`: local time of day, so Denver time on hoid. `BACKUP_KEEP=14`.
- `SLACK_BOT_TOKEN`: empty disables the status message. `SLACK_STATUS_CHANNEL=finbot-finances`: the bot must be a member.
- `MCP_PUBLIC_URL` (bare origin), `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`, `MCP_ALLOWED_EMAILS`
  (comma-separated): MCP is disabled unless all four are set.
