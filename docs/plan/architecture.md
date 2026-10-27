# Personal Finance Assistant — Architecture & Build Plan (a4 — agreed by Claude + agy, pending Brendan's approval)

Status: PLANNING ONLY. Drafted by Claude for adversarial review by agy, then Brendan.
Implements requirements.md v5. IDs (R*, N*, V*) refer to that file.

## 0. Ground truth this design builds on
- Repo `YNAB_AutoCategorizer`: ~3.2k lines of Go (1.3k are tests). Packages: categorizer (guardrails), AI (Vertex/OpenAI), search + datastore (Turso vector), ynab (ynab.go v1.5.0), config, migrate (embedded SQL migrations). Single binary, Dockerfile (CGO_ENABLED=0), runs on a 6h ticker.
- hoid: Ubuntu 24.04-ish x86_64, Docker Compose apps (kids-ledger, chore-chart) on SQLite, nginx-proxy-manager with Let's Encrypt already in front of them, nightly local-only SQLite backup cron for kids-ledger.

## 1. Shape: one Go binary, one container, one SQLite file
Evolve the repo into a single long-running service `finbot` (name TBD) with subcommands for ops/one-offs:

```
finbot serve        # scheduler + MCP HTTP server + Slack socket client (the daemon)
finbot sync         # one-off delta sync
finbot calibrate    # classifier bake-off/threshold calibration on archived plan
finbot seed-plan    # fresh-start structure + seeding proposal (dry-run by default)
finbot audit        # re-review categorizations
finbot backup       # snapshot + encrypted off-site push
```

Deployed as a Docker Compose project on hoid like kids-ledger, data volume holds `finbot.db`. Rationale: N5 (evolve, Go), N2 (fewest moving parts), N1 (data on hoid). No Postgres, no Turso, no Grafana.

### Package layout (evolution of the current repo)
| Package | Status | Role |
|---|---|---|
| `ynab/` | rewrite | Thin wrapper over `pkg.venceslau.dev/ynab` v1.6.1 (V4). Rate-limit governor (token bucket, 200/hr, writes reserved headroom). |
| `store/` | new (absorbs `migrate/`) | SQLite via `modernc.org/sqlite` (pure Go, keeps CGO off). Embedded migrations. Opened with `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON` (sync, MCP and Slack goroutines share it). |
| `sync/` | new | Delta sync of the active plan via `GET /plans/{id}?last_knowledge_of_server=N` (one call returns all changed entities; transactions arrive as `TransactionSummaryBase` with `subtransactions` as a separate top-level array — stitch by transaction_id, join names locally). Archived plan synced once / on demand. Triggers bank import. Detects human edits (R6b). |
| `classify/` | replaces `AI/`, `search/`, `datastore/`, absorbs `categorizer/` | Pipeline: guardrails (`classify/guardrails.go`, existing tests ported) → deterministic rules → model. Calibrated thresholds. Decision log. |
| `budget/` | new | CSP structure, fresh-start seeding, paycheck assignment rule (R7b), goals (R8b), tithing math (R12). |
| `intents/` | new | Store conversational intents; deterministic matcher run after each sync (R8a). |
| `audit/` | new | Re-review decisions (R6c). |
| `alerts/`, `report/` | new | Anomaly rules (R11/R11a), monthly check-in (R10). |
| `mcp/` | new | Remote MCP server (R9, R9a), write tiers (R7a). |
| `auth/` | new | OAuth for MCP; user identities (N7, N9). |
| `slack/` | new | Socket Mode client: questions, answers, alerts, reports (R9b). |
| `llm/` | new | Runs the `claude` CLI headless (Q15) for free-text understanding. See §5a. |
| `writer/` | new | The ONLY path that writes to YNAB. Write log with before/after, actor, tier, request id. Revert support. |

## 2. Data model (SQLite)
- Mirror tables per plan_id: accounts (+ local `tax_bucket` tag), category_groups, categories (+targets), payees, transactions, subtransactions, month_categories, scheduled_transactions; `sync_state(plan_id, server_knowledge, synced_at)`.
- `decisions`: txn_id, chosen category, confidence, runner-up, method (rule/model), model+version, input features, created_at, superseded_by.
- `write_log`: id, actor (system / brendan / wife), via (scheduler / mcp / slack), tier, endpoint, txn/category ids, before_json, after_json, request_id, reverted_by.
- `human_locks`: (txn_id, field) diverged from our last write → never overwrite (R6b). Also emitted as labeled training examples.
- `intents`: id, user, text, parsed hints (category, amount, merchant, date), status (pending/matched/asked/expired), matched_txn_ids.
- `questions`: id, kind, slack ts, options, status, answer, answered_by.
- `users`: brendan, wife — slack_user_id, oauth subject, fun-money category id.
- `profile`: paycheck matcher, pre-tax deductions, retirement assumptions, assignment amounts (filled at first run, Q11).

## 3. Core flows

### 3.1 Sync (every 15 min → 4 req/hr; + import every 6h)
0. Every ~6h: `POST /plans/{id}/transactions/import` (equivalent of pressing Import in the app — without it, linked accounts may not import if nobody opens YNAB; verified endpoint exists, background-import behavior otherwise unverified).
1. Delta GET the active plan; upsert, tombstone deletes.
2. Human-edit detection: (a) txn with a write_log entry whose fields diverge from our last `after_json` → `human_locks`; (b) txn with no write_log entry that is `approved=true` with a category → human-categorized on first touch → `human_locks`. (Unapproved txns with a category are treated as YNAB's own payee-memory guess, not human — UNVERIFIED that YNAB auto-fills on import; confirm in M1 by observation.) Both → labeled examples.
3. Emit events: new txns, paycheck detected, link errors (`direct_import_in_error`, V3), staleness.
4. Freshness = per-account last import time + `synced_at`; every MCP answer includes it (R11a).

### 3.2 Categorize (after each sync)
1. Guardrails: transfers / CC payments → skip; locked → skip.
2. Rules: exact learned payee→category from history and human corrections (cheap, high precision).
3. Model: constrained choice over active category list (name + note + group), with payee, amount, date, account, memo, and the payee's last N labels as context. Output: probability per category.
4. If p_top ≥ τ (calibrated for 85% precision, R6) → writer applies (routine tier, approved=true). Else → Slack question with top-3 buttons (R9b).
5. Every outcome → `decisions`.

### 3.3 Classifier choice & calibration (spike first — U1)
- `classify.Model` interface. Candidates: **Jev** (Brendan's credits; unverified API — U1) as primary; **headless `claude` CLI** (§5a) as fallback/second opinion, batching many txns per call to conserve subscription usage. Vertex/OpenAI code paths are removed.
- `finbot calibrate`: run candidates over the archived plan's labeled history mapped old→new categories (mapping table proposed by AI, approved by Brendan once), compute precision/coverage curves, pick model + τ. Model-reported confidence is never trusted raw — τ comes from the measured curve.
- Turso/vector double-hop is deleted once calibration shows constrained choice is ≥ as precise (expected).

### 3.4 Audit (R6c)
- Weekly scheduled: re-score decisions made in the last 90 days with the current model + current rules + new human labels; disagreements with margin → auto-fix (routine tier, logged) or Slack question if low confidence. Never touches `human_locks`.
- MCP `audit_transactions(filter)` lets either user ask for it ad hoc ("re-check all Costco categorizations").

### 3.5 Budget ownership
- Assigning money is one `PATCH months/{month}/categories/{id}` per category (no bulk endpoint, verified). A paycheck across ~20 categories = ~20 requests: writer runs it as a resumable job (each step logged; re-read Ready to Assign before each step; resume after partial failure).
- Fresh start: Brendan clicks Fresh Start in YNAB (V9). `finbot seed-plan` then (dry-run → approved once): create CSP groups + categories + targets (V2), map old→new, compute seeding (goals already earmarked, fun-money balance from Aug 2026 using old plan spending), cap at Ready to Assign.
- Paycheck event → assign in R7b order, capped at Ready to Assign, routine tier, logged, Slack FYI.
- Tithing (R12): tithe_owed = 10% × Σ net paycheck deposits since last tithing payment − partials.

### 3.6 Intents (R8a)
- Logged via MCP or Slack under the speaker's identity ("my fun money" → that user's category).
- Matcher after each sync: candidates = unlocked, uncategorized-or-auto-categorized txns within [intent date −1d, +5d], filtered by hints, excluding txns already claimed by another intent. Prefer the speaker's own cards when accounts have an owner tag. One candidate → apply + Slack FYI. Several (incl. both spouses' intents competing for similar charges) or zero after window → Slack question. Expire at 10 days.

### 3.7 Alerts & reports
- Rules engine in Go, evaluated after sync: missing/short paycheck (vs. paycheck matcher cadence), recurring price change (same payee, >10% vs trailing median), duplicate (same payee+amount within 72h, different import ids), large vs same-payee history, tithing behind, link error/stale.
- Alert budget enforced in code: non-critical alerts beyond ~2/month roll into the monthly report.
- Monthly check-in (1st of month) and December +1% nudge → Slack.

## 4. Remote MCP (R9, N6, N7, N9)
- Go: official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`, Streamable HTTP transport) — version to verify (U8).
- Exposure: a `finance.` subdomain on the domain nginx-proxy-manager already serves, with Let's Encrypt (Q16 decided).
- Auth (Q17 decided): MCP spec OAuth 2.1 with login delegated to Google Sign-In, allowlist of exactly two Google accounts (no passwords stored). Do NOT hand-roll an OAuth server (agy, accepted). M0 spike picks one of: (a) a battle-tested library-based AS (e.g. `ory/fosite`) kept minimal inside finbot, (b) an external AS in front (e.g. Cloudflare Access for MCP, if exposure goes via Cloudflare), (c) whatever auth helpers the chosen MCP Go SDK ships. Selection criteria: works with Claude.ai custom connectors end-to-end, revocation in one step, smallest code we own. Rate-limit + fail2ban on /mcp and /oauth at the proxy. Which AI apps (Claude web/mobile/desktop, ChatGPT, Gemini) support remote MCP + OAuth must be verified (U9).
- Tools (examples):
  - Read: `get_financial_snapshot` (R9a), `query_transactions(filter)`, `compare_spending(period, baseline)`, `get_income_events`, `get_tithing_status`, `get_goals`, `get_fun_money`, `get_category_decisions(txn)`, `data_freshness`.
  - Routine write: `categorize_transactions`, `set_memo`, `log_intent`, `answer_question`, `audit_transactions`, `set_goal_amount`.
  - Structural write (R7a): `delete_transaction`, `create_category`, `move_assigned_money`, `bulk_recategorize` (> N), `revert_write`. Two-step: first call returns a preview + one-time confirmation token bound to user + args + 10-min expiry; second call with token executes. Enforced server-side.
- Every tool response includes data freshness; every write includes write_log id (revertable).

## 5. Slack (R9b, R10, R11, N9)
- Slack app in **Socket Mode** (outbound websocket from hoid; no public Slack endpoint). Library `github.com/slack-go/slack`.
- One channel (e.g. #finances) shared by both spouses + DMs. Slack user id → `users`.
- Questions use Block Kit buttons (top-3 categories, "other…", "split") → deterministic handling, no LLM needed.
- Free-text replies/intents in Slack ("I bought discs with my fun money") go to the headless `claude` CLI (§5a), which acts through finbot's own MCP tools as the speaking user.

## 5a. Server-side LLM: headless `claude` CLI (Q15)
- Two call sites, two privilege levels (agy, accepted):
  - **Classifier fallback (§3.3):** `claude -p --output-format json --tools "" --strict-mcp-config` (no MCP at all) + `--json-schema` for the category choice. Payee/memo text can't trigger anything because no tools exist.
  - **Slack free-text (§5):** `--tools ""`, `--strict-mcp-config --mcp-config <generated>` pointing ONLY at finbot's MCP on localhost, `--allowedTools` limited to the read tools + routine writes relevant to conversation, `--permission-prompts none` (anything else is auto-denied instead of hanging the worker), `--setting-sources ""`-equivalent (no user/project settings), `--disable-slash-commands`, `--append-system-prompt-file`.
  - NOT `--bare`: under `--bare` auth is API-key only (OAuth tokens never read) — verified in `--help`.
- Conversation continuity: `--resume <session-id>` per Slack thread, with `--system-prompt-snapshot off`, and every user message prefixed `[Speaker: brendan|wife]` so a thread both spouses reply in resolves "my fun money" correctly. The MCP bearer token is minted per invocation for the speaking user; M0 must verify `--resume` picks up the new `--mcp-config` each turn.
- Prompt injection residual risk: Venmo/bank memos are counterparty-controlled text that enters the Slack session's context. Mitigations: classifier path has zero tools; Slack path can only do routine writes (all logged + revertable, audited); structural writes need a human Slack button enforced by finbot. Accepted residual.
- Identity: each invocation's MCP token is bound to the Slack user → same tiers (R7a) and attribution (N9) as AI apps.
- Auth on hoid: Brendan runs `claude setup-token` once → `CLAUDE_CODE_OAUTH_TOKEN` in `.env`. Token TTL unverified → the health check includes a tiny `claude -p` auth probe daily so expiry alerts Slack immediately.
- Install: native `claude` build baked into the finbot image at a PINNED version; upgrades deliberate + smoke-tested. `~/.claude` (sessions, state) lives on the persistent volume so restarts keep `--resume` working. hoid currently has neither claude nor node.
- Ops: single-worker queue; Slack shows "thinking…" (expect 5–30s); timeout + one retry; failures → plain Slack error, never silent.
- Risks: shares Brendan's subscription usage limits with his interactive use (U11); whether the subscription terms cover this personal automated use must be confirmed (U12).

## 6. Reliability, security, ops
- N2 60-day test: healthchecks.io (or similar) dead-man ping after each successful sync; it alerts Slack if hoid/finbot goes silent — the system can't report its own death.
- N4 backup: nightly SQLite `.backup` → `age`-encrypted → off-site (Backblaze B2 or Google Drive) via restic or rclone. Optionally fix kids-ledger's local-only backup the same way.
- N3 secrets: `.env` on hoid with 0600, not in git; YNAB PAT, Jev key, Slack tokens, OAuth signing key, backup key.
- Concurrency with human edits: YNAB has no optimistic concurrency, so writer re-syncs immediately before each write batch and PATCHes only the fields it changes, minimizing the clobber window.
- Prompt-injection: v1 has no untrusted-content ingestion (Amazon deferred). Payee/memo strings are passed to models as data, never instructions; structural writes need confirmation regardless.

## 7. Build plan (milestones, each ends deployed + verified on hoid)
Order principle (agy): get read-only value and visibility BEFORE the irreversible Fresh Start and before autonomous writes.
- M0 Spikes (~1 session): Jev API access/latency/output shape (U1); MCP Go SDK choice (official go-sdk vs mark3labs/mcp-go) + OAuth option end-to-end with a Claude.ai custom connector (U8, U9); Slack Socket Mode hello-world; headless `claude` CLI smoke test on hoid (setup-token, MCP-only tools, resume, JSON output, latency) (U11, U12).
- M1 Read-only mirror: library upgrade, SQLite store, delta sync + import trigger, compose deploy on hoid, dead-man healthcheck, encrypted off-site backup. Old 6h categorizer disabled.
- M2 MCP read tools + OAuth + public exposure, over the CURRENT plan. Both spouses can ask questions from their AI apps.
- M3 Classifier + Slack: calibrate on history, rules + model, decision log, writer + write_log + human locks; Slack questions/answers; alerts + freshness. Runs in dry-run (Slack shows what it WOULD do) for ~1 week, then auto at τ.
- M4 Budget ownership: Brendan clicks Fresh Start; seed-plan (dry-run → approved once); CSP structure; paycheck assignment job; tithing.
- M5 MCP write tools + tiers, intents, audit job, monthly check-in report + December nudge.
- v1.1 Amazon enrichment.

## 8. Decisions
- Q15: server LLM = headless `claude` CLI on Brendan's subscription. Q16: existing nginx-proxy-manager domain. Q17: Google Sign-In, two accounts.

## 9. Unverified (to resolve in M0)
- U1 Jev. U8 official MCP Go SDK maturity/transport. U9 which AI apps (Claude, ChatGPT, Gemini — web and mobile) support remote MCP with OAuth and DCR. U10 healthchecks/Slack integration specifics. U11 subscription usage headroom for automated calls. U12 subscription terms for personal automated `claude -p` use.
