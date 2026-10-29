# Backlog

The supervisor turns entries into briefs (`tasks/<ID>.md`). M0 and M1 are detailed. M2 to M5 are expanded at each milestone
gate, after Brendan says continue. "Touches" entries are exclusive: never run two tasks with overlapping Touches at the same
time. BA-n refers to *Brendan actions* in STATE.md.

## M0: Spikes (settle the unknowns; throwaway code lives under `spikes/<name>/`, not in product packages)

Milestone acceptance:
- Each spike writes `docs/build/spikes/<name>.md` with:
  - findings, labeled VERIFIED or UNVERIFIED;
  - a recommendation;
  - an "impact on architecture" section.
- sup brings any architecture impact to Brendan before M1.

| ID | Title | Depends | Touches | Needs | Notes |
|---|---|---|---|---|---|
| M0-1 | Jev classifier spike | none | spikes/jev | BA-1 | API shape, auth, latency, calibrated-probability output over about 40 categories. Run 100 historical txns read-only. Cost per 1k txns. Covers U1. |
| M0-2a | MCP SDK + OAuth choice (local) | none | spikes/mcp | BA-5 | Compare official go-sdk and mark3labs/mcp-go: Streamable HTTP, auth hooks. Pick an OAuth approach without hand-rolling (fosite, an external AS, or SDK helpers). Run a hello tool with Google sign-in locally; verify the flow with MCP Inspector. Research which AI clients support remote MCP with OAuth and DCR (U8, U9). |
| M0-2b | MCP hello on hoid + connector | M0-2a | spikes/mcp | BA-3, BA-4 | Deploy the M0-2a hello server at `finance.<domain>` on hoid behind nginx-proxy-manager. Brendan adds it as a claude.ai custom connector; verify on web and mobile, and in his wife's AI app if possible. |
| M0-3 | Slack Socket Mode spike | none | spikes/slack | BA-2 | Using slack-go: post to #finances, Block Kit buttons with the callback received, threaded reply received, Slack user id → identity. |
| M0-4 | Headless claude on hoid spike | none | spikes/claude | BA-6 | Install the native claude in `~/.local/bin` on hoid (no sudo). Brendan runs `claude setup-token`. Verify the §5a flags on hoid: `--tools ""`, strict MCP config, `--json-schema`, `--permission-prompts none`, `--resume` picking up a new mcp-config, `--system-prompt-snapshot off`, latency. Research U11 (usage) and U12 (terms) with sources. |
| M0-5 | YNAB read-only probe | none | spikes/ynab | none (.env) | With `pkg.venceslau.dev/ynab` v1.6.1: full plan fetch, size and timing, delta fetch, the subtransaction stitching shape, account link fields. Observe whether imported txns arrive pre-categorized and unapproved (YNAB payee memory) to settle the §3.1 assumption. Strictly read-only. |

M0-5 can start immediately; M0-1, M0-2a and M0-3 start as soon as their BA item is done. M0-2b and M0-4 wait on their BA
items. Run at most 3 at once.

## M1: Read-only mirror on hoid

Milestone acceptance:
- finbot runs on hoid in Docker Compose.
- The SQLite mirror of the active plan stays current: a delta every 15 minutes, an import every 6 hours.
- The archived-plan one-shot sync works.
- The dead-man healthcheck pings.
- A nightly encrypted off-site backup exists and is restore-tested.
- The old 6-hour categorizer is off.
- No writes to YNAB.

| ID | Title | Depends | Touches | Notes |
|---|---|---|---|---|
| M1-1 | YNAB client upgrade + rate governor | M0-5 | ynab/, go.mod, categorizer/ (compile fixes only) | Replace ynab.go v1.5.0 with `pkg.venceslau.dev/ynab` v1.6.1. Token bucket of 200/hr with write headroom. Existing tests stay green. M0 notes: root module to Go 1.26; install the zone-normalizing transport for `goal_snoozed_at` (spikes/ynab.md F1); redact the plan id from library errors (arch §3.1). |
| M1-2 | store package | none | store/, migrate/ → store | modernc sqlite. WAL, busy_timeout, foreign_keys. Embedded migrations. Mirror schema plus sync_state (§2). |
| M1-3 | sync package | M1-1, M1-2 | sync/ | Delta GET of the plan, subtxn stitching, tombstones, the import trigger, `finbot sync` and the archived-plan one-shot (§3.1, steps 0 and 1). Human-lock detection waits for M3. |
| M1-4 | finbot serve skeleton + config | M1-3 | main.go, cmd/, config/ | Subcommands (§1), in-process scheduler, structured logs, `/healthz`. Remove the 6-hour ticker path. |
| M1-5a | Deploy on hoid + healthcheck | M1-4 | deploy/, Dockerfile | A compose project in `~/finbot` on hoid, a data volume, `.env` handling, a healthchecks.io ping (BA-7). Deploy: yes. M0 notes: glibc Debian-slim runtime image (not Alpine/distroless static); split 0600 env files, `finbot.env` + `tunnel.env` (arch §6); deploy script fails loudly (`pipefail`, no `\| tail`), asserts env files non-empty + 0600, rsync excludes `*.env`. |
| M1-5b | Encrypted off-site backup + restore test | M1-5a | deploy/backup | Nightly SQLite `.backup`, then age, then an off-site push (BA-8), and a documented restore test. Deploy: yes. |

## M2: MCP read tools + OAuth + public exposure (current plan) [expand at gate]

Covers §4 read tools, `get_financial_snapshot`, data freshness, account tax-bucket tags, and auth for two users. Uses M0-2's
choice.

M0 notes (arch §2, §4, §6):
- `mcp/` + `auth/` from `spikes/mcp/` (go-sdk v1.8.0 + fosite v0.49.0 + go-oidc).
- Persistent fosite storage in SQLite + `token_generations`, and a persisted HMAC secret (redeploys keep sign-ins).
- Cloudflare rate-limiting rules on `/oauth/*` and `/mcp`: block-only, never a challenge, skip `160.79.104.0/21`.
- Container hardening (agy, M0-2b review): `cap_drop: [ALL]`, `no-new-privileges`, `read_only`, log rotation,
  cloudflared `--metrics 127.0.0.1:20241`, pinned base images.
- The wife's real Google sign-in through a connector (allowlist her account; UNVERIFIED in M0).

## M3: Classifier + Slack (dry-run, then live) [expand at gate]

Covers the §3.2 to §3.4 calibration (old to new category mapping, approved by Brendan), rules + model, the decision log, the
writer and write_log, human locks, Slack questions, alerts, freshness, and the classifier fallback via headless claude (§5a).
One week of dry-run, then auto at τ, with Brendan's go-ahead.

## M4: Budget ownership [expand at gate]

Brendan runs Fresh Start (BA). Covers `seed-plan` (dry-run, approved once), the CSP structure, the resumable paycheck
assignment job, and tithing.

## M5: Writes, intents, audit, reports [expand at gate]

Covers MCP write tools with tiers and the confirmation flow, Slack free text via headless claude, intents, the audit job, and
the monthly check-in plus December nudge.

## v1.1: Amazon enrichment [after v1]
