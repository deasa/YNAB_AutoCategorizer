# Personal Finance Assistant — Architecture & Build Plan (a4 — agreed by Claude + agy, pending Brendan's approval)

Status: PLANNING ONLY. Drafted by Claude for adversarial review by agy, then Brendan. Updated at the M0 gate (2026-10-29) with the spike results (`docs/build/spikes/`) and Brendan's M0 decisions.
Implements requirements.md v5. IDs (R*, N*, V*) refer to that file.

## 0. Ground truth this design builds on
- Repo `YNAB_AutoCategorizer`: ~3.2k lines of Go (1.3k are tests). Packages: categorizer (guardrails), AI (Vertex/OpenAI), search + datastore (Turso vector), ynab (ynab.go v1.5.0), config, migrate (embedded SQL migrations). Single binary, Dockerfile (CGO_ENABLED=0), runs on a 6h ticker. Root module moves to **Go 1.26** in M1 (`slack-go` v0.32.0 and the M0 spike modules need it).
- hoid: Ubuntu 24.04-ish x86_64, Docker Compose apps (kids-ledger, chore-chart) on SQLite, nightly local-only SQLite backup cron for kids-ledger. The router does not forward 80/443 to hoid; nginx-proxy-manager is installed but unused. Domain `hoidlabs.site` has its DNS on Cloudflare, so public exposure is a Cloudflare Tunnel (§4).

## 1. Shape: one Go binary, one container, one SQLite file
Evolve the repo into a single long-running service `finbot` (name TBD) with subcommands for ops/one-offs:

```
finbot serve        # scheduler + MCP HTTP server + Slack socket client (the daemon)
finbot sync         # one-off delta sync
finbot calibrate    # classifier bake-off/threshold calibration on archived plan
finbot seed-plan    # fresh-start structure + seeding proposal (dry-run by default)
finbot audit        # re-review categorizations
finbot backup       # snapshot + encrypted off-site push
finbot revoke       # revoke one user's (or all) MCP tokens, no restart (N7)
```

Deployed as a Docker Compose project on hoid like kids-ledger, data volume holds `finbot.db`. Two services: `finbot` and a `cloudflared` sidecar (§4); neither publishes a host port. The finbot runtime image is glibc-based (Debian slim, e.g. `debian:bookworm-slim`), because the native `claude` binary (§5a) links against glibc; no Alpine/musl or `distroless/static`. Rationale: N5 (evolve, Go), N2 (fewest moving parts), N1 (data on hoid). No Postgres, no Turso, no Grafana.

### Package layout (evolution of the current repo)
| Package | Status | Role |
|---|---|---|
| `ynab/` | rewrite | Thin wrapper over `pkg.venceslau.dev/ynab` v1.6.1 (V4), with a zone-normalizing HTTP transport for `goal_snoozed_at` and plan-id redaction in errors (§3.1). Rate-limit governor (local token bucket, 200/hr, writes reserved headroom). |
| `store/` | new (absorbs `migrate/`) | SQLite via `modernc.org/sqlite` (pure Go, keeps CGO off). Embedded migrations. Opened with `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON` (sync, MCP and Slack goroutines share it). |
| `sync/` | new | Delta sync of the active plan via `GET /plans/{id}?last_knowledge_of_server=N` (one call returns all changed entities; transactions arrive as `TransactionSummaryBase` with `subtransactions` as a separate top-level array — stitch by transaction_id, join names locally). Archived plan synced once / on demand. Triggers bank import. Detects human edits (R6b). |
| `classify/` | replaces `AI/`, `search/`, `datastore/`, absorbs `categorizer/` | Pipeline: guardrails (`classify/guardrails.go`, existing tests ported) → deterministic rules → model. Calibrated thresholds. Decision log. |
| `budget/` | new | CSP structure, fresh-start seeding, paycheck assignment rule (R7b), goals (R8b), tithing math (R12). |
| `intents/` | new | Store conversational intents; deterministic matcher run after each sync (R8a). |
| `audit/` | new | Re-review decisions (R6c). |
| `alerts/`, `report/` | new | Anomaly rules (R11/R11a), monthly check-in (R10). |
| `mcp/` | new | Remote MCP server (R9, R9a) on go-sdk, write tiers (R7a). Lands in M2. |
| `auth/` | new | OAuth AS for MCP (fosite + Google via go-oidc), user identities, revocation (N7, N9). Lands in M2. |
| `slack/` | new | Socket Mode client: questions, answers, alerts, reports (R9b). |
| `llm/` | new | Runs the `claude` CLI headless (Q15) for free-text understanding. See §5a. |
| `writer/` | new | The ONLY path that writes to YNAB. Write log with before/after, actor, tier, request id. Revert support. |

## 2. Data model (SQLite)
- Mirror tables per plan_id: accounts (+ local `tax_bucket` tag), category_groups, categories (+targets), payees, transactions, subtransactions (keyed by `id`, FK `transaction_id`), month_categories, scheduled_transactions; `sync_state(plan_id, server_knowledge, synced_at)`.
  - A split parent's `category_id` is a sentinel that is not in `categories`: no FK from transactions to categories (or map the sentinel to NULL + `is_split`). Category reads and aggregations use `subtransactions`, never the split parent's sentinel `category_id`.
- `decisions`: txn_id, chosen category, confidence, runner-up, method (rule/model), model+version, input features, created_at, superseded_by.
- `write_log`: id, actor (system / brendan / wife), via (scheduler / mcp / slack), tier, endpoint, txn/category ids, before_json, after_json, request_id, reverted_by.
- `human_locks`: (txn_id, field) diverged from our last write → never overwrite (R6b). Also emitted as labeled training examples.
- `intents`: id, user, text, parsed hints (category, amount, merchant, date), status (pending/matched/asked/expired), matched_txn_ids.
- `questions`: id, kind, subject id (e.g. txn id), slack `channel_id` + `message_ts` (lookup key for clicks and thread replies, survives restarts), options, status, answer, answered_by, answered_at. One table; no separate `slack_questions`.
- `users`: brendan, wife — `slack_user_id` (Slack identity; no email), OAuth subject `u_<sha256(email)[:12]>`, display name, fun-money category id.
- OAuth (M2, `auth/`): fosite storage tables (DCR clients, auth codes, access/refresh tokens, PKCE sessions) + `token_generations(user, generation)` for one-step revocation. The fosite HMAC secret is persisted, so a redeploy doesn't log everyone out.
- `profile`: paycheck matcher, pre-tax deductions, retirement assumptions, assignment amounts (filled at first run, Q11).

## 3. Core flows

### 3.1 Sync (every 15 min → 4 req/hr; + import every 6h)
0. Every ~6h: `POST /plans/{id}/transactions/import` (equivalent of pressing Import in the app — without it, linked accounts may not import if nobody opens YNAB; verified endpoint exists, background-import behavior otherwise unverified).
1. `Plan.Delta(&SyncState)` is the only sync read (one request per tick, ~0.2 s, <1 KB when idle); initial load = full export, then deltas. Upsert; honor `deleted` in both paths (tombstones appear only in deltas, but deleted categories also appear in full exports).
2. Human-edit detection: (a) txn with a write_log entry whose fields diverge from our last `after_json` → `human_locks`; (b) txn with no write_log entry that is `approved=true` with a category → human-categorized on first touch → `human_locks`. (Unapproved txns with a category are treated as YNAB's own payee-memory guess, not human — M0 saw 9/9 such txns match a prior approved payee→category and the old categorizer is believed off; still confirm in M1 by observing new imports.) Both (a) and (b) → labeled examples. Unapproved txns already in `Inflow: Ready to Assign` are skipped by §3.2.
3. Emit events: new txns, paycheck detected, link errors (`direct_import_in_error`, V3; deduped, so the first sync doesn't spam Slack with the cards already in error), staleness.
4. Freshness: YNAB exposes no import timestamp, so per-account freshness = `max(date) WHERE import_id IS NOT NULL` plus the local `synced_at` when a new `import_id` first appears; every MCP answer includes it (R11a).
- Client notes (`ynab/`): `pkg.venceslau.dev/ynab` v1.6.1 can't decode the live plan's zone-less `goal_snoozed_at`; M1-1 installs a zone-normalizing HTTP transport. Library errors embed the plan id → wrap and redact every error. YNAB sends no `X-Rate-Limit` header → the 200/hr budget is a local counter.

### 3.2 Categorize (after each sync)
1. Guardrails: transfers / CC payments → skip; P2P payments → skip (a real P2P detector: most P2P payees don't contain "Venmo", so no substring match); existing splits (split parents / subtransactions) → skip (V6: an existing split can't be routine-written; changing it is delete + recreate, structural tier per R7a); unapproved txns already in `Inflow: Ready to Assign` → skip (never overwrite an inflow/paycheck with an expense category); locked → skip.
2. Rules: unanimous learned payee→category from history and human corrections (cheap, high precision).
3. Model: constrained choice over the active classifier-target category list (name + description + group), with payee, amount, date, account, memo, and the payee's last N human labels as context. Description = the YNAB note, or a policy override from config (e.g. the household `Discretionary` policy), versioned with calibration. Output: probability per category.
4. If p_top ≥ τ (calibrated for 85% precision, R6; starts at 0.8) → writer applies (routine tier, approved=true). Else → Slack question with top-3 buttons (R9b).
5. A failed model call, or a null structured result, is an error, not "uncertain": the txn is skipped and retried next sync, never mapped to a Slack question or a default.
6. Every outcome → `decisions`.
- Category targets: purpose/event categories (Birthday, Trip, Christmas, Giving, …) are savings set-asides only and are never classifier targets (excluded like Birthday/Gift today). Spending for an event is categorized `Discretionary`.

### 3.3 Classifier choice & calibration
- Primary: **Jev** (`jev-1.14.0`, model id pinned, never `jev-latest`), run only with the rules layer and payee history in state plus the `Discretionary` policy. M0 (n=200): τ=0.8 on p_top → ~90% precision / ~73% coverage; the top-3 holds the right answer 91–93% of the time. Bare Jev (no history/policy) is rejected. **Headless `claude` CLI** (§5a) is the fallback/second opinion on the τ-miss band. Vertex/OpenAI code paths are removed.
- Interface is batch-first (Jev and the claude CLI both want batches of ~10+):
  ```go
  type Txn struct {
      ID, Payee, Memo, Account string
      Date                     time.Time
      AmountMilli              int64
      PayeeHistory             []string // last N human labels for this payee, newest first
  }
  type Candidate struct{ Name, Group, Description string } // Description = YNAB note or policy override
  type Prediction struct {
      Probs map[string]float64 // per candidate; gate on p_top (Jev's own confidence adds nothing)
      Top   []string           // ordered; top-3 → Slack buttons
      Model string             // versioned id, logged in `decisions`; τ is valid only for this version
  }
  type Model interface {
      Classify(ctx context.Context, txns []Txn, cands []Candidate) ([]Prediction, error)
  }
  ```
- `finbot calibrate`: run candidates over the archived plan's labeled history mapped old→new categories (mapping table proposed by AI, approved by Brendan once), compute precision/coverage curves, pick model + τ. Model-reported confidence is never trusted raw — τ comes from the measured curve, re-derived on the whole archived history before the routine tier auto-approves, and re-measured whenever the policy text or category list changes. Older/archived plans that contain a `Discretionary` category reflect the current categorizing strategy and are valid labelled data (calibration, Jev re-measure, payee history); the approval gap since 2026-04 is a lapse, not a strategy change.
- Category list: Brendan finalizes it later. Until then builders work from a **PROPOSED** simplified set (his transaction history + Conscious Spending Plan buckets + stated wants: per-spouse fun money, travel, generosity, auto, event set-asides). Label it PROPOSED wherever it appears; nothing renames or creates live YNAB categories without Brendan.
- Turso/vector double-hop is deleted once calibration shows constrained choice is ≥ as precise (expected).

### 3.4 Audit (R6c)
- Weekly scheduled: re-score decisions made in the last 90 days with the current model + current rules + new human labels; disagreements with margin → auto-fix (routine tier, logged) or Slack question if low confidence. Never touches `human_locks`.
- MCP `audit_transactions(filter)` lets either user ask for it ad hoc ("re-check all Costco categorizations").

### 3.5 Budget ownership
- Assigning money is one `PATCH months/{month}/categories/{id}` per category (no bulk endpoint, verified). A paycheck across ~20 categories = ~20 requests: writer runs it as a resumable job (each step logged; re-read Ready to Assign before each step; resume after partial failure).
- Fresh start: Brendan clicks Fresh Start in YNAB (V9). `finbot seed-plan` then (dry-run → approved once): create CSP groups + categories + targets (V2), map old→new, compute seeding (goals already earmarked, fun-money balance from Aug 2026 using old plan spending), cap at Ready to Assign.
- Paycheck event → assign in R7b order, capped at Ready to Assign, routine tier, logged, Slack FYI.
- Event set-asides (Birthday, Trip, Christmas, Giving, …) are CSP savings goals that accumulate money for the event; spending isn't tracked per event. Zeroing a set-aside after the event is **never automatic**: Brendan does it in YNAB, or asks an AI through a user-initiated M5 MCP write tool under the normal confirmation flow.
- Tithing (R12): tithe_owed = 10% × Σ net paycheck deposits since last tithing payment − partials.

### 3.6 Intents (R8a)
- Logged via MCP or Slack under the speaker's identity ("my fun money" → that user's category).
- Matcher after each sync: candidates = unlocked, uncategorized-or-auto-categorized txns within [intent date −1d, +5d], filtered by hints, excluding txns already claimed by another intent. Prefer the speaker's own cards when accounts have an owner tag. One candidate → apply + Slack FYI. Several (incl. both spouses' intents competing for similar charges) or zero after window → Slack question. Expire at 10 days.

### 3.7 Alerts & reports
- Rules engine in Go, evaluated after sync: missing/short paycheck (vs. paycheck matcher cadence), recurring price change (same payee, >10% vs trailing median), duplicate (same payee+amount within 72h, different import ids), large vs same-payee history, tithing behind, link error/stale.
- Alert budget enforced in code: non-critical alerts beyond ~2/month roll into the monthly report.
- Monthly check-in (1st of month) and December +1% nudge → Slack.

## 4. Remote MCP (R9, N6, N7, N9)
- Go: official MCP Go SDK `github.com/modelcontextprotocol/go-sdk` v1.8.0 (spec 2026-07-28), Streamable HTTP transport. `tools/list` returns only the tools the caller's token permits.
- Exposure (Q16): `finance.hoidlabs.site` via a **Cloudflare Tunnel**. A `cloudflared` sidecar in the finbot compose project reaches `finbot:8080` over the compose network; Cloudflare terminates TLS. finbot publishes no host port and listens on the container interface (`MCP_LISTEN_ADDR=:8080`), never host loopback (the go-sdk DNS-rebinding guard would 403 the public Host). hoid needs no inbound ports, certificates or sudo. `cloudflared` metrics bind to `127.0.0.1`.
- Auth (Q17): MCP spec OAuth 2.1 with login delegated to Google Sign-In, allowlist of exactly two Google accounts (verified email; no passwords stored). No hand-rolled OAuth or crypto. Design: an `ory/fosite` v0.49.0 AS inside finbot + go-sdk resource-server helpers + Google sign-in via `go-oidc`: DCR (not CIMD), PKCE S256 only, CSRF-protected consent page, opaque HMAC access tokens with introspection, refresh rotation, RFC 7009 revocation, and per-user token generations so `finbot revoke` is one step without a restart. Subject = `u_<sha256(email)[:12]>`, mapped to a display name for N9 attribution. Storage is persistent SQLite + a persisted HMAC secret from M2. Cloudflare Access (external AS) is rejected: MCP clients need standard OAuth discovery.
- Abuse limits: Cloudflare **rate-limiting rules** on `/oauth/*` and `/mcp`, block-only (never a managed challenge, which API callers can't solve) and skipping Anthropic's egress range `160.79.104.0/21`; plus the in-app caps (DCR clients 500, pending logins 100). No fail2ban (every request arrives from cloudflared). If finbot logs client IPs, it reads `CF-Connecting-IP` and trusts it only from the cloudflared container. If Bot Fight Mode / Under Attack is ever enabled, add a WAF skip rule for the hostname.
- Clients (U9): claude.ai web and Claude mobile VERIFIED live against the hoid deployment (incl. revoke → reconnect → re-auth). ChatGPT and Gemini UNVERIFIED.
- Tools (examples):
  - Read: `get_financial_snapshot` (R9a), `query_transactions(filter)`, `compare_spending(period, baseline)`, `get_income_events`, `get_tithing_status`, `get_goals`, `get_fun_money`, `get_category_decisions(txn)`, `data_freshness`.
  - Routine write: `categorize_transactions`, `set_memo`, `log_intent`, `answer_question`, `audit_transactions`, `set_goal_amount`.
  - Structural write (R7a): `delete_transaction`, `create_category`, `move_assigned_money`, `bulk_recategorize` (> N), `revert_write`. Two-step: first call returns a preview + one-time confirmation token bound to user + args + 10-min expiry; second call with token executes. Enforced server-side.
- Every tool response includes data freshness; every write includes write_log id (revertable).

## 5. Slack (R9b, R10, R11, N9)
- Slack app in **Socket Mode** (outbound websocket from hoid; no public Slack endpoint). Library `github.com/slack-go/slack` v0.32.0 (needs Go 1.26). Reconnect needs no app code.
- One private channel (`#finbot-finances`) shared by both spouses + DMs. The app needs the bot event `message.groups` (without it, private-channel replies are silently dropped); Scopes: app-level token `connections:write`; bot `chat:write`, `groups:read`, `groups:history`, `users:read`; drop `groups:write` (and the `channels:*` scopes while finbot stays private-channel-only). DMs additionally need `im:history`, `im:write` and the `message.im` event (UNVERIFIED). Slack user id → `users` (no email lookup).
- Handling rules: ack every envelope first, then handle async; ignore the bot's own messages (`bot_id` set) and any `subtype` other than none / `thread_broadcast`; make clicks and replies idempotent (key on `event_id`, or `container.message_ts` + `action_ts`), since redelivery after reconnect is plausible; route clicks and thread replies via `questions.(channel_id, message_ts)` and `chat.update` the question once answered.
- Startup self-check: the configured channel must resolve via `users.conversations`, else alert.
- Questions use Block Kit buttons (top-3 categories, "other…", "split") → deterministic handling, no LLM needed.
- Free-text replies/intents in Slack ("I bought discs with my fun money") go to the headless `claude` CLI (§5a), which acts through finbot's own MCP tools as the speaking user.

## 5a. Server-side LLM: headless `claude` CLI (Q15)
- Every call: absolute path to the pinned `claude` 2.1.312, `--model haiku`, prompt on **stdin** (never argv: variadic flags like `--tools` swallow a positional prompt), cwd = an empty dedicated directory, env `DISABLE_UPDATES=1 CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 CLAUDE_CODE_DISABLE_CLAUDE_MDS=1 CLAUDE_CODE_OAUTH_TOKEN=…`, and **no** `ANTHROPIC_API_KEY` in the container env (it would take precedence).
- Two call sites, two privilege levels (agy, accepted):
  - **Classifier fallback (§3.3):** `claude -p --model haiku --output-format json --tools "" --strict-mcp-config --setting-sources "" --disable-slash-commands --no-session-persistence --json-schema <schema>` (no MCP at all). Payee/memo text can't trigger anything because no tools exist. Success ⇔ `rc==0 && !.is_error && .structured_output != null`; anything else (incl. a schema rejection, which returns `is_error:false` with null output) → skip the txn. Batch ~10 txns per call (`{results:[{id,category,p}]}`) and check every id comes back exactly once. No `--max-turns 1` with `--json-schema`.
  - **Slack free-text (§5):** `--tools ""`, `--strict-mcp-config --mcp-config <file>` pointing ONLY at finbot's MCP on localhost, `--allowedTools` limited to the read tools + routine writes relevant to conversation, **`--permission-mode dontAsk`** (required: `auto` mode approved a write outside `--allowedTools` in M0), `--permission-prompts none` (anything else is auto-denied instead of hanging the worker), `--setting-sources ""` (no user/project settings, so nothing can set `defaultMode`), `--disable-slash-commands`, `--append-system-prompt-file` (hidden flag; fall back to `--append-system-prompt` if it misbehaves), `--system-prompt-snapshot off`. The `--mcp-config` is a 0600 temp file, deleted after the call, so the bearer token is never on argv. Record `.permission_denials` for audit.
  - NOT `--bare`: under `--bare` auth is API-key only (OAuth tokens never read) — verified in `--help`.
- Conversation continuity: `--resume <session-id>` per Slack thread (`.session_id` → thread mapping), with `--system-prompt-snapshot off`, and every user message prefixed `[Speaker: brendan|wife]` so a thread both spouses reply in resolves "my fun money" correctly. The MCP bearer token is minted per invocation for the speaking user; `--resume` with a new `--mcp-config` each turn is VERIFIED for a stdio MCP stub only; HTTP MCP with an `Authorization` header across `--resume` must be integration-tested in M3/M5 (`llm/`).
- Prompt injection residual risk: Venmo/bank memos are counterparty-controlled text that enters the Slack session's context. Mitigations: classifier path has zero tools; Slack path can only do routine writes (all logged + revertable, audited); structural writes need a human Slack button enforced by finbot. Accepted residual.
- Identity: each invocation's MCP token is bound to the Slack user → same tiers (R7a) and attribution (N9) as AI apps.
- Auth on hoid: Brendan runs `DISABLE_UPDATES=1 claude setup-token` once → `CLAUDE_CODE_OAUTH_TOKEN` in finbot's env file (§6). Token TTL is documented as ~1 year but unverified → a daily health probe (`claude -p` replying "ok") passes only if `rc==0 && !.is_error && trimmed lowercase .result == "ok"`; anything else alerts Slack with `api_error_status` (a bad token gives rc 1, `is_error:true`, 401).
- Install: native `claude` build baked into the glibc finbot image at a PINNED version (**2.1.312**; the installer takes an exact version); `DISABLE_UPDATES=1` in the image; upgrades deliberate + smoke-tested. `~/.claude` (sessions, state) and `~/.claude.json` both live on the persistent volume so restarts keep `--resume` working.
- Ops: single-worker queue; Slack shows "thinking…"; M0 measured ~3 s per classifier call, ~5 s per batch of 10, ~3–4.5 s per Slack turn with 1–2 tool calls (stub MCP; real tools add their own time); timeout + one retry; failures → plain Slack error, never silent.
- Subscription: capacity is sufficient and this personal use is within the terms (U11, U12 closed by Brendan).

## 6. Reliability, security, ops
- N2 60-day test: healthchecks.io (or similar) dead-man ping after each successful sync; it alerts Slack if hoid/finbot goes silent — the system can't report its own death.
- N4 backup: nightly SQLite `.backup` → `age`-encrypted → off-site (Backblaze B2 or Google Drive) via restic or rclone. Optionally fix kids-ledger's local-only backup the same way.
- N3 secrets: split 0600 env files on hoid, not in git, each service seeing only its own keys: `finbot.env` (YNAB PAT, Jev key, Slack tokens, Google OAuth client, `CLAUDE_CODE_OAUTH_TOKEN`, backup key; never `ANTHROPIC_API_KEY` or the tunnel token) and `tunnel.env` (`TUNNEL_TOKEN` only, for cloudflared). The deploy script fails on any error (no output-truncating pipes without `pipefail`), asserts each generated env file is non-empty and 0600, and excludes `*.env`/`.env` from rsync.
- Container hardening (both services): non-root, no host ports, `cap_drop: [ALL]`, `security_opt: [no-new-privileges:true]`, `read_only: true` (writable `/data` volume only), bounded `logging` (`max-size`/`max-file`), base images pinned by digest or patch version.
- Setup checklist: Slack app has Socket Mode on and the `message.groups` bot event subscribed; Google OAuth client has `https://finance.hoidlabs.site/oauth/google/callback`; Cloudflare rate-limit rules (§4) in place.
- Concurrency with human edits: YNAB has no optimistic concurrency, so writer re-syncs immediately before each write batch and PATCHes only the fields it changes, minimizing the clobber window.
- Prompt-injection: v1 has no untrusted-content ingestion (Amazon deferred). Payee/memo strings are passed to models as data, never instructions; structural writes need confirmation regardless.

## 7. Build plan (milestones, each ends deployed + verified on hoid)
Order principle (agy): get read-only value and visibility BEFORE the irreversible Fresh Start and before autonomous writes.
- M0 Spikes: done 2026-10-29 (Jev, MCP + OAuth + Cloudflare Tunnel, Slack, headless `claude`, YNAB probe). Results are folded into this doc; details in `docs/build/spikes/`.
- M1 Read-only mirror: library upgrade (Go 1.26, zone-normalizing transport), SQLite store, delta sync + import trigger, compose deploy on hoid (glibc image, split env files), dead-man healthcheck, encrypted off-site backup. Old 6h categorizer disabled.
- M2 MCP read tools + OAuth (`mcp/` + `auth/`, persistent fosite SQLite storage + persisted HMAC secret) + public exposure via the Cloudflare Tunnel with rate-limit rules, over the CURRENT plan. Both spouses can ask questions from their AI apps.
- M3 Classifier + Slack: calibrate on history, rules + model, decision log, writer + write_log + human locks; Slack questions/answers; alerts + freshness. Runs in dry-run (Slack shows what it WOULD do) for ~1 week, then auto at τ.
- M4 Budget ownership: Brendan clicks Fresh Start; seed-plan (dry-run → approved once); CSP structure; paycheck assignment job; tithing.
- M5 MCP write tools + tiers, intents, audit job, monthly check-in report + December nudge.
- v1.1 Amazon enrichment.

## 8. Decisions
- Q15: server LLM = headless `claude` CLI on Brendan's subscription, Haiku, pinned 2.1.312 (§5a).
- Q16: public exposure = Cloudflare Tunnel (`cloudflared` sidecar) at `finance.hoidlabs.site`; Cloudflare rate-limit rules replace fail2ban (§4). Replaces the nginx-proxy-manager plan.
- Q17: Google Sign-In, two accounts, via a fosite AS + go-sdk + go-oidc inside finbot (§4).
- Classifier: Jev primary behind rules + payee history, τ starts at 0.8 (§3.3).
- Categories: event set-asides are savings only, event spending → `Discretionary`, zeroing never automatic (§3.2, §3.5); the simplified category list is PROPOSED until Brendan finalizes it (§3.3).

## 9. Unverified
- Closed in M0: U1 (Jev viable as primary, §3.3), U8 (go-sdk v1.8.0), U11 + U12 (Brendan: capacity sufficient, use within terms). YNAB delta/tombstone/link-field behavior verified (§3.1).
- U9 narrowed: Claude web + mobile VERIFIED live; ChatGPT and Gemini UNVERIFIED (not needed for v1).
- Still open: U10 healthchecks/Slack integration specifics. YNAB payee memory on import (§3.1 step 2; confirm in M1). Wife's real Google sign-in (M2). `claude` OAuth token TTL (daily probe covers it, §5a).
