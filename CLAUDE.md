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
./finbot calibrate [--limit N] [--batch N] [--out path]   # fit τ on labelled history; needs FINBOT_DB_PATH, YNAB_BUDGET_ID, JEV_API_KEY
./finbot dry-run-report        # how the dry run did, from the local DB; needs only FINBOT_DB_PATH; last line `writes: <n>`
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
JEV_LIVE=1 go test ./classify/jev -run TestLive -v           # one small Jev batch; needs JEV_API_KEY
CLAUDE_LIVE=1 go test ./llm -run TestLive -v                 # probe + one classify call; needs CLAUDE_CODE_OAUTH_TOKEN, CLAUDE_BIN
YNAB_SANDBOX_WRITE=1 go test ./writer -run TestSandbox -v    # writes to YNAB_SANDBOX_PLAN_ID only; refuses YNAB_BUDGET_ID
```
`finbot calibrate` writes `calibrate/calibration.json` by default: run it from the repo root so it lands on the embedded
file, then rebuild for the new τ to take effect.
Set `YNAB_LIVE_SYNC_LOG=<path>` to keep the live sync log. Grep any live output for UUIDs, the token and the plan id
before showing it.

Docker and deploy:
```bash
docker build --build-arg GIT_SHA=$(git rev-parse --short HEAD) -t finbot .
docker run --env-file .env -v finbot-data:/data finbot serve
./deploy/deploy.sh                     # deploy to hoid, from the repo root, then wait for healthy
./deploy/test-preflight.sh             # deploy.sh's .env preflight against dummy .env files; git and ssh are stubs
./deploy/rehearse-docker.sh            # real image, compose file, listeners and cloudflared flags, dummy credentials
```
The image runs as uid 10001. The `GIT_SHA` build arg sets `cmd.Version` (default `dev`), shown in the Slack status
message. The image bakes in `claude` 2.1.312 at `/usr/local/bin/claude` (installed under `/opt/claude`, outside the data
volume). Bumping it means changing the `CLAUDE_VERSION` build arg, plus `CLAUDE_INSTALLER_SHA256` if the installer
changed. `deploy.sh` ships `git archive HEAD` (it refuses uncommitted tracked changes; `spikes`, `docs` and env files
stay out) to `hoid:~/finbot/src` and writes `~/finbot/finbot.env` (0600) from the finbot keys in the local `.env`. It
runs compose project `finbot` with `deploy/compose.yaml`, which sets `TZ=America/Denver`, `IMPORT_ENABLED=true`,
`HTTP_ADDR=127.0.0.1:8081`, `CLASSIFY_MODE=dry_run`, `CLAUDE_BIN=/usr/local/bin/claude` and
`CLAUDE_CONFIG_DIR=/data/claude`. `deploy.sh` refuses an `.env` with no value for the Google, allowlist, Slack
(`SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, `SLACK_USERS`), `JEV_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` keys, and refuses one
that sets `ANTHROPIC_API_KEY` (it would outrank the OAuth token).

`cloudflared` is compose profile `tunnel`: `deploy.sh` enables it only when the marker `~/finbot/tunnel-active` exists
on hoid (it does; the M2 cutover set it), and then first checks `~/finbot/tunnel.env` (0600, exactly one
`TUNNEL_TOKEN=` line). The cutover and rollback scripts were retired in M3 once the spike was removed.

## Architecture

finbot mirrors YNAB plans into one local SQLite file and keeps the active plan current with one delta request per tick.
After each successful sync it categorizes new transactions (rules, then Jev, with a headless `claude` second opinion),
applying only above a calibrated threshold in `auto` and otherwise asking in Slack. It also sends alerts to Slack.

**Package roles:**
- `main.go` → `cmd.Main`: subcommand dispatch (stdlib `flag`).
- `cmd/` — the subcommands and the scheduler. `Scheduler` runs the sync job immediately, then every `SYNC_INTERVAL`; the
  import job every `IMPORT_INTERVAL` only when `IMPORT_ENABLED`; and the backup job nightly at `BACKUP_AT` when backups
  are configured. The first import after a start comes at `last_attempt_at + IMPORT_INTERVAL` (immediately if overdue; a
  full interval with no prior attempt), and each attempt is stored in `import_state` (a rate-budget skip records
  nothing). When MCP is on, a prune job deletes expired OAuth rows at startup and every 24h; its outcome never touches
  `Health`. When the claude second opinion is configured and the mode isn't off, a probe job runs `claude` once after
  the first sync and then every 24h: a failure sends the critical `claude_auth` alert (with `api_error_status`), a pass
  clears it, and it never touches `Health`. The Slack status update runs after each scheduled sync, import and backup,
  as part of that job (`finbot sync` and `finbot backup` never touch it). All jobs run on one goroutine, so ticks never
  overlap; a job's own tick that comes due while it runs is dropped; other jobs' ticks run right after. Each scheduled
  sync also sends the optional dead-man ping (`HEALTHCHECK_PING_URL`). After a successful scheduled sync only (not a
  failed or budget-skipped one), serve feeds the freshness and anomaly alerts, then categorizes: `writer.Reconcile`
  (intents older than this sync's start), the `classify` pipeline (unless the mode is off), then `Questions.CloseStale`,
  all under one 5-min bound with a reserve kept for `CloseStale`. Neither touches `Health`. Serve has one
  `writer.Writer` and one `writer.Gate` (starting at `dry_run`), shared by the pipeline and Slack, and syncs are
  serialized. The label plans (`calibrate.LabelPlans`: the active plan plus every scanned plan with `Discretionary`) are
  resolved on every run, so a `sync --archived` takes effect without a restart; if that lookup fails, the gate fails
  closed to `dry_run`. With Socket Mode on, serve upserts `users` and runs the Slack self-check before the first sync: a
  failure sends the critical `socket_self_check` alert and retries with backoff, and sync carries on. `Health` serves
  `GET /healthz` on its own private listener (`HTTP_ADDR`), since it returns `last_error` and the tunnel forwards all of
  the MCP port: 200 with `{"last_sync_ok","last_error"}` while a sync succeeded within 2×`SYNC_INTERVAL`, else 503 (a
  rate-budget skip counts as not fresh). Logs are `log/slog` JSON; `sync`/`ynab`'s `*log.Logger` is adapted with
  `slog.NewLogLogger`. Graceful shutdown on SIGINT/SIGTERM cancels the running tick and closes the Slack client, the
  HTTP servers and the DB; the waits share one 5 s deadline, and past it serve logs and exits 1.
- `config/` — env loading (godotenv for `.env`). `LoadFrom(getenv)` for tests. Errors name the variable, never the value.
- `store/` — SQLite via `modernc.org/sqlite` (pure Go, CGO off), WAL, separate writer/reader pools, embedded migrations
  in `store/migrations/` (`0001_mirror`, `0002_plan_scan`, `0003_status_message`, `0004_plan_scan_keep_verdict`,
  `0005_oauth`, `0006_decisions`, `0007_write_log_locks`, `0008_questions`, `0009_lock_detection`). Database files are
  kept 0600. Mirror tables per plan, `sync_state` cursor, `plan_scan` (persisted archived scan; read with
  `DB.PlanScans`), `status_message` (the Slack message and last backup outcome, kept across restarts), `decisions`,
  `write_log`, `human_locks`, `questions`, `users`, `slack_events`, `alerts`, `import_state`, `alert_cursor` and
  `lock_detection` (one-time lock backfill per plan). Timestamps are fixed-width UTC strings. Read queries for MCP:
  split-aware `QueryLines` and `CategoryTotals` in `store/transactions_read.go`; `Accounts`, `MonthlyFlows`,
  `HistoryRange`, `Goals` and `IncomeLines` in `store/snapshot_read.go`. Income excludes transfers in from off-budget
  accounts that YNAB categorizes to Ready to Assign (moved savings, not income). Classifier reads and writes:
  `store/decisions.go`, `store/labeled_read.go` (labelled history; finbot's own writes count only with a category lock),
  `store/mirror_read.go`, `store/writelog.go`, `store/questions.go`, `store/alerts.go`, and `store/report_read.go`
  (`ReportPlans`, `CurrentHumanCategory` for the dry-run report).
- `sync/` — `Syncer.SyncPlan`: full export the first time a plan is seen, then one delta per tick; rows and cursor
  commit in one transaction. Stitches subtransactions, emits bank-link `LinkEvent`s on transitions, counts orphans.
  Detects human edits for the active plan in the sync transaction (`human_locks`, with a one-time backfill per plan):
  a field that differs from finbot's last `done` write is locked (`ynab_edit`), and an approved, really categorized row
  with no finbot write gets a category lock (`first_touch`); rows with an unresolved intent are skipped. The counts go
  on the `sync tick` log line as `human_locks`. `ScanDiscretionary` scans every plan (archived included) and persists
  the result. `TriggerImport` is the bank-import POST. Nothing logged carries an id.
- `ynab/` — thin wrapper over `pkg.venceslau.dev/ynab` (V4) with a zone-normalizing, body-capping HTTP transport
  (`goal_snoozed_at`). A local rate governor (200/hr, write reserve) runs as the library's `Limiter`; write intent
  travels on ctx (`asWrite`). Every error has the token, plan id and other ids redacted. The only YNAB writes are
  `ImportTransactions`, `PatchTransactions` (one bulk PATCH of `category_id`/`approved`, called only by `writer/`) and
  the four category writes `CreateCategoryGroup`, `RenameCategoryGroup`, `CreateCategory` and `UpdateCategory` (name,
  group, goal target and date), called only by the manual one-off `finbot restructure`, never by serve.
  `NotApplied(err)` says a write provably changed nothing.
- `writer/` — the only path that categorizes transactions in YNAB. `Apply` holds one mutex for the whole call (the
  scheduler and Slack both call it), checks the hard gates (`Gate` mode must be `auto`, the plan, `CLASSIFY_AUTO_SINCE`)
  under it, re-syncs the mirror, re-reads each row (skips missing, deleted, locked, split, changed since the decision,
  before AutoSince), logs an `intent` in `write_log` with the whole before/after state, PATCHes, then marks each row
  `done` or `failed` from YNAB's answer. An answer chosen in Slack also stores a category lock before `done`.
  `Reconcile` settles intents whose answer was lost, after a successful sync. In any mode but `auto`, `Apply` returns
  `ErrDryRun` and writes nothing.
- `classify/` — picks a category for each new transaction: guardrails (`Guard`: transfers, credit card payments, P2P,
  splits, Ready to Assign inflows, locks, no payee or memo; in `auto`, also anything dated before AutoSince), then
  unanimous payee rules (at least 3 labels), then a model over the active plan's candidates (`Candidates`; set-asides,
  hidden, deleted and internal categories and the Credit Card Payments group are out), with
  the embedded `policy.json` (the `Discretionary` policy). The policy, candidates and model are hashed so a calibration
  applies only to what it measured. `Pipeline.Run` runs once per successful sync: it sets the effective mode first
  (`off` does nothing; `auto` needs a calibration that is `auto_eligible` with a τ and matching hashes, otherwise it runs
  as `dry_run` with a warning; a failure loading candidates publishes `dry_run`), then takes up to 50 eligible
  transactions from the last `CLASSIFY_BACKLOG_DAYS`, newest first. Each gets one decision per model, hashes and mode
  (logged in `decisions`, older ones superseded). In `auto`, a model answer at or above τ (default 0.8) is applied
  through `writer` (actor `system`, via `scheduler`, approved); a miss gets the `claude` second opinion, which is
  advisory only (logged and shown in the question, never written), and becomes a `categorize` question. In `dry_run`
  every decision becomes a `dry_run_check` question instead. Questions are posted oldest first while fewer than
  `CLASSIFY_MAX_OPEN_QUESTIONS` are open; the rest wait as `pending`. Entering `auto` closes the dry-run questions
  (`superseded_by_auto`). A failed model call skips the transaction until the next sync.
- `classify/jev/` — the Jev `classify.Model`. The model is pinned to `jev-1.14.0` (a response from any other version
  fails the batch), batches of 10, one retry on 5xx or timeout. Payee, memo and history travel only as request values;
  errors never carry the key, payee, memo or an id.
- `llm/` — runs the pinned headless `claude` CLI: `Classifier` (the second opinion, a `classify.Model`, batches of 10)
  and `Probe` (the daily auth check). Every call uses the absolute `CLAUDE_BIN`, `--model haiku`, the prompt on stdin,
  an empty dedicated cwd and an env built from an allowlist (so `ANTHROPIC_API_KEY` never reaches it), one worker, a
  90 s timeout and one retry. Errors carry fixed text, exit codes and API status codes only: never the token, prompt or
  output.
- `calibrate/` — `finbot calibrate` runs labelled history oldest first through the runtime's guard, rules and Jev, and
  fits τ: the smallest value whose Wilson-95 lower bound on the tuning 70% reaches 0.85 precision. `auto_eligible` also
  needs at least 200 tuning model-band rows and holdout precision of 0.85. The result is the checked-in, embedded
  `calibration.json` (`Current()`, `Applies`). History comes from `LabelPlans`; set-aside labels are dropped and labels
  outside the candidates count as hard negatives. `BuildDryRunReport` backs `finbot dry-run-report`: each `would_apply`
  or asked decision is judged by the next human label (an answered question, or a human-set category in the mirror).
- `slack/` — the Socket Mode client: acks every envelope first, drops bot messages and subtypes, accepts only the
  `SLACK_USERS` people in the configured channel, dedupes redeliveries in `slack_events`, `SelfCheck` via
  `users.conversations`, and Post/Update. `Questions` renders a question (top-3 buttons, other, split), routes clicks and
  thread replies by `(channel, message_ts)`, and stores the answer as a category id. A `categorize` answer goes through
  `writer.Apply` as that person; a `dry_run_check` answer is only recorded. "Split" tells the user to split in YNAB and
  closes the question (`split_in_ynab`); the next sync locks the row. `CloseStale` closes questions whose transaction
  was deleted or locked. `status/` keeps its own small Slack client.
- `alerts/` — `Alerter.Send` is the one entry point. Critical kinds (`link_error`, `stale_import`, `claude_auth`,
  `socket_self_check`) always send; other kinds past 2 sent per calendar month are stored `rolled_up` and never posted.
  A failed post deletes the record so the next tick retries. `Freshness` runs after each successful sync: a link error,
  and a linked account with no import for over 14 days (the same rule as the MCP's `no_recent_import`), each alert once
  per episode and re-arm when they clear. `Anomalies` runs deterministic rules over rows first seen since
  `alert_cursor` (the first run is a silent baseline): `duplicate` (same payee and amount at most 3 days apart, different
  import ids), `price_change` (a recurring bill more than 10% off its trailing median) and `large_charge` (far above the
  payee's usual charge, with at least 5 earlier ones). Inflows, transfers and deleted rows are skipped; one alert per
  transaction. Missing or short paycheck and tithing alerts come in M4.
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
  `data_freshness`, `query_transactions`, `compare_spending`, `get_financial_snapshot`, `get_goals`, `get_income_events`,
  `get_category_decisions` (a transaction's decision log).
  Every response carries data freshness: `last_synced_at`, `age_seconds`, `stale` (older than 2×`SYNC_INTERVAL` or never
  synced), `links_ok`, `accounts`, `warnings`; a linked account with no import for over 14 days is flagged
  `no_recent_import`. `badRequest` is the only way to send a client a specific error, built from the caller's input and
  fixed text; everything else reaches the client as a generic failure. Snapshot averages are day-precise when history
  starts mid-window (total × 30.4375 / days), so a partial first month doesn't deflate them.
- `status/` — keeps one Slack message current, edited after every scheduled sync, import and backup; it's a heartbeat,
  and Brendan's own Slack automation alerts when it goes stale. It shows the last import's result and the classifier
  mode, with a warning when a configured `auto` ran as something else; a failed import or such a downgrade marks it
  degraded (Slack only; `/healthz` is untouched). Slack failures are only logged and never fail the job. Each update is
  capped at 20 s, plus a 5 s store write that outlives the run's ctx. A new message is posted at most once per 6h.

**Key data flow:** `finbot serve` → `Scheduler` tick → `Syncer.SyncPlan(activePlan)` → `ynab.Client.Export`/`Delta` →
`store` upsert, human-lock detection and cursor in one tx → slog line (initial/delta, skipped, row counts, orphans, link
events by account name, `human_locks` counts) → ping → on success: freshness and anomaly alerts, then
`writer.Reconcile` → `classify.Pipeline.Run` → `Questions.CloseStale` (5-min bound) → Slack status edit, if configured →
`Health.OK`/`Fail`.

## Safety

- YNAB writes go through three paths only: `ImportTransactions` (the bank import), `writer/` (`PatchTransactions`, the
  only path that categorizes) and `finbot restructure` (the four category writes `CreateCategoryGroup`,
  `RenameCategoryGroup`, `CreateCategory`, `UpdateCategory`; run by hand, never by serve). `restructure --apply` on the
  live plan needs `--confirm-live` and Brendan's OK for the exact printed list; nothing else calls the category
  writes. The `IMPORT_ENABLED` config default stays false; the hoid deploy pins it true in
  `deploy/compose.yaml` (Brendan approved, BA-13, 2026-11-08), so that deploy POSTs to the live plan every
  `IMPORT_INTERVAL`. Turning it on anywhere else against the live plan still needs Brendan's OK.
- `CLASSIFY_MODE` defaults to `off`; the hoid deploy pins `dry_run`, which decides and asks but never writes. Setting
  `CLASSIFY_MODE=auto` (with `CLASSIFY_AUTO_SINCE`) against the live plan is Brendan's decision alone: no builder,
  supervisor or reviewer turns it on. Auto writes only transactions dated on or after `CLASSIFY_AUTO_SINCE`, never the
  backlog. Writer tests run against the sandbox plan (`YNAB_SANDBOX_PLAN_ID`) only.
- Never log or print ids, the token or the plan id; plan and account names are fine. `HEALTHCHECK_PING_URL`,
  `BACKUP_TURSO_URL`, `BACKUP_TURSO_TOKEN`, `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN`, `GOOGLE_OAUTH_CLIENT_SECRET`,
  `JEV_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN` and `TUNNEL_TOKEN` are secrets too. Allowlisted emails and the Slack user ids
  in `SLACK_USERS` are scrubbed from logs like secrets (`secrets()` in `cmd/app.go`). Never set `ANTHROPIC_API_KEY` in
  finbot's env: it would outrank the OAuth token.
- `./finbot serve` or `docker run --env-file .env` with the real `.env` acts on real services: a fresh DB posts a new
  status message to the real channel, Socket Mode answers and alerts in the real channel, a classify mode other than
  `off` calls Jev and `claude` and posts questions, and a backup that comes due goes to the real `finbot-backup` and
  prunes it. Blank `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` and `BACKUP_*`, and leave `CLASSIFY_MODE` off, for local runs.

## Testing

External dependencies sit behind interfaces with fakes (`sync.PlanAPI`, `sync.PlanLister`, an injectable `Ticker` and
clock in `cmd`, `classify.Model`/`Applier`/`Asker`, `writer.API`, `slack.API`/`Socket`, `llm.Execer`, the alerts
`Poster`). Store tests use a temp SQLite file. Live tests are env-gated (above).

## Configuration

All config is via environment variables (loaded from `.env`); see `.env.example`. Two exceptions: `finbot health` reads
`HTTP_ADDR` from the real environment only, and `deploy.sh` forwards only `YNAB_ACCESS_TOKEN`, `YNAB_BUDGET_ID`,
`HEALTHCHECK_PING_URL`, `BACKUP_*`, `SLACK_*`, `GOOGLE_OAUTH_*`, `MCP_ALLOWED_EMAILS`, `JEV_API_KEY` and
`CLAUDE_CODE_OAUTH_TOKEN` (never `ANTHROPIC_API_KEY`), so other keys in `.env` (such as `SYNC_INTERVAL`,
`IMPORT_INTERVAL`, `LOG_LEVEL`, `HTTP_ADDR`, `MCP_PUBLIC_URL`) never reach hoid; `deploy/compose.yaml` sets
the hoid values of `HTTP_ADDR`, `MCP_LISTEN_ADDR`, `MCP_PUBLIC_URL`, `CLASSIFY_MODE`, `CLAUDE_BIN` and `CLAUDE_CONFIG_DIR`.
Required: `YNAB_ACCESS_TOKEN`, `YNAB_BUDGET_ID` (the active plan). Defaults: `FINBOT_DB_PATH=/data/finbot.db`,
`SYNC_INTERVAL=15m`, `IMPORT_INTERVAL=6h`, `IMPORT_ENABLED=false`, `HTTP_ADDR=127.0.0.1:8081`, `MCP_LISTEN_ADDR=:8080`,
`LOG_LEVEL=info`, `CLASSIFY_MODE=off`. A local `.env` copied from an old example may still have `HTTP_ADDR=:8080`:
remove that line, or with MCP on `serve` fails at startup with "address already in use".

Optional:
- `HEALTHCHECK_PING_URL`: dead-man ping after each scheduled sync. Empty disables it.
- `BACKUP_AGE_RECIPIENT`, `BACKUP_TURSO_URL`, `BACKUP_TURSO_TOKEN`: backups are disabled unless all three are set.
- `BACKUP_AT=03:17`: local time of day, so Denver time on hoid. `BACKUP_KEEP=14`.
- `SLACK_BOT_TOKEN`: empty disables the status message. `SLACK_STATUS_CHANNEL=finbot-finances`: the bot must be a member.
- `MCP_PUBLIC_URL` (bare origin), `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET`, `MCP_ALLOWED_EMAILS`
  (comma-separated): MCP is disabled unless all four are set.
- `SLACK_APP_TOKEN` (`xapp-`, a secret) and `SLACK_USERS` (`brendan:<slack user id>,hilarie:<slack user id>`; names map
  to store labels brendan→brendan, hilarie or wife→wife, each label once; any other name is a config error): Socket Mode
  and the alerts it posts are off unless both Slack tokens and `SLACK_USERS` are set.
- `CLASSIFY_MODE` (`off`, `dry_run` or `auto`; anything else fails startup). `JEV_API_KEY` is required unless it is off.
  `auto` also requires `CLASSIFY_AUTO_SINCE` (`YYYY-MM-DD`). `CLASSIFY_BACKLOG_DAYS=45`, `CLASSIFY_MAX_OPEN_QUESTIONS=5`.
- `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_BIN` and `CLAUDE_CONFIG_DIR` (both absolute paths): the `claude` second opinion and
  its daily probe run only when the token and `CLAUDE_BIN` are set.
