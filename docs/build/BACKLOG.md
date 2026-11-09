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
| M1-4 (→ M1-4a, M1-4b) | finbot serve skeleton + config | M1-3 | main.go, cmd/, config/ | Subcommands (§1), in-process scheduler, structured logs, `/healthz`. Remove the 6-hour ticker path. |
| M1-5a | Deploy on hoid + healthcheck | M1-4 | deploy/, Dockerfile | A compose project in `~/finbot` on hoid, a data volume, `.env` handling, a healthchecks.io ping (BA-7). Deploy: yes. M0 notes: glibc Debian-slim runtime image (not Alpine/distroless static); split 0600 env files, `finbot.env` + `tunnel.env` (arch §6); deploy script fails loudly (`pipefail`, no `\| tail`), asserts env files non-empty + 0600, rsync excludes `*.env`. |
| M1-5b | Encrypted off-site backup + restore test | M1-5a | deploy/backup | Nightly SQLite `.backup`, then age, then an off-site push (BA-8), and a documented restore test. Deploy: yes. |
| M1-5c | Slack status message (heartbeat) | M1-5b | status/, cmd/ | One message in #finbot-finances edited after each sync/backup; replaces healthchecks.io (BA-7, Brendan 2026-11-01). |

## M2: MCP read tools + OAuth + public exposure (current plan) [expanded 2026-11-04]

Covers §4 read tools, `get_financial_snapshot`, data freshness, account tax-bucket tags, and auth for two users. Briefs are
in `tasks/`. Deferred because their data doesn't exist yet: `get_category_decisions` (M3), `get_tithing_status` and
`get_fun_money` (M4).

| ID | Title | Depends | Touches | Notes |
|---|---|---|---|---|
| M2-0 | Carried M1 cleanup | none | cmd/ (comment), store/, sync/ | plan_scan overwrite, ScanError.Unwrap, DB files 0600. Deploy: yes. |
| M2-1a | Persistent OAuth storage | M2-0 | auth/, store/ | fosite on SQLite, token_generations, persisted HMAC secret, DCR eviction. |
| M2-1b | Authorization server port | M2-1a | auth/ | Spike AS, Google sign-in, consent, allowlist. |
| M2-2 | MCP server in serve | M2-1b | mcp/, cmd/, config/ | data_freshness, `finbot revoke`, `/healthz` kept off the public port. |
| M2-3 | Transaction read tools | M2-2 | mcp/, store/ | query_transactions, compare_spending. |
| M2-4 | Snapshot and tax-bucket tags | M2-3 | mcp/, store/, cmd/ | get_financial_snapshot, get_goals, get_income_events, tag-account. |
| M2-5 | Tunnel cutover from finbot-spike | M2-4 | deploy/, Dockerfile | finance.hoidlabs.site stays up; rollback script. Deploy: yes. |
| M2-docs | Docs follow-ups | M2-5 | CLAUDE.md, docs/plan | Gate docs task. |
| M2-acceptance | Independent test | M2-docs | tests/acceptance/M2/ | Black-box, by agy. Includes the wife's real sign-in (BA-16). |

## M3: Classifier + Slack (dry-run, then live) [expanded 2026-11-08]

Covers §3.2 and §3.3 rules + model, calibration, the decision log, the writer and write_log, human locks, Slack questions,
alerts, freshness, and the classifier second opinion via headless claude (§5a). Dry-run for at least a week, then auto at τ
behind a flag only Brendan turns on. Briefs are in `tasks/`. M3 classifies into the active plan's **existing** categories
(Brendan, 2026-11-08): no old→new remap task and no category creation. The PROPOSED simplified list stays an M4 input.

| ID | Title | Depends | Touches | Notes |
|---|---|---|---|---|
| M3-0a | Carried sync and ynab cleanup | none | sync/, ynab/ | ScanDiscretionary keeps a failed plan with a good verdict; check op strings carry no ids. |
| M3-0b | Retire cutover rollback | none | deploy/ | The spike is gone, so rollback.sh has nothing to return to. |
| M3-1a | Decision log and labelled history | none | store/ | `decisions` (0006), LabeledHistory, decision reads. |
| M3-1b | write_log and human_locks | M3-1a | store/ | Migration 0007, accessors. |
| M3-1c | Questions, users, alerts, import state | M3-1b | store/ | Migration 0008, accessors. |
| M3-2 | Guardrails, rules, candidates | M3-1b | classify/ | §3.3 interface, b2414be guardrail port, P2P detector, policy file. |
| M3-3 | Jev model client | M3-2 | classify/jev/ | Implements `classify.Model`, pinned `jev-1.14.0`. |
| M3-4 | Headless claude runner | M3-2 | llm/ | §5a classifier site and daily auth probe only. |
| M3-5 | finbot calibrate | M3-3 | calibrate/, cmd/ | Labelled set, curves, τ, `calibration.json`. |
| M3-6 | Run calibration | M3-5 | calibrate/ | Data file only. YNAB read-only, Jev live. |
| M3-7 | Writer | M3-0a, M3-1b | writer/, ynab/ | The only categorize write path; fakes only. |
| M3-8 | Writer sandbox test | M3-7 | writer/ | Env-gated, sandbox writes only (BA-9 done). |
| M3-9 | Human-lock detection | M3-0a, M3-1b | sync/ | §3.1 step 2. |
| M3-10a | Slack Socket Mode core | M3-1c | slack/, go.mod | Connect, filter, identity, dedupe, self-check. |
| M3-10b | Slack questions | M3-10a | slack/ | Block Kit, clicks, thread replies, close-out. |
| M3-11 | Categorize pipeline | M3-3, M3-4, M3-1c | classify/ | Modes off, dry_run, auto; question cap. |
| M3-12a | Freshness alerts and budget | M3-1c | alerts/ | Link error, stale import, alert budget. |
| M3-12b | Anomaly alerts | M3-12a | alerts/ | Duplicate, price change, large vs payee history. |
| M3-13 | get_category_decisions | M3-1a | mcp/ | Deferred from M2. |
| M3-14a | Slack and alerts in serve | M3-5, M3-10b, M3-12a | cmd/, config/ | Includes the bounded shutdown wait. |
| M3-14b | Categorize job in serve | M3-14a, M3-11, M3-7, M3-9 | cmd/, config/ | `CLASSIFY_MODE`, default off. |
| M3-15 | Import outcomes and timer | M3-14b | cmd/, status/ | Import result in the status message; timer survives restarts. |
| M3-16 | Dry-run report | M3-15, M3-6 | calibrate/, cmd/ | `finbot dry-run-report`. |
| M3-17 | Deploy in dry-run | M3-16, M3-8, M3-12b, M3-13, M3-0b | deploy/, Dockerfile | claude in the image, env forwards, `CLASSIFY_MODE=dry_run`. Deploy: yes. |
| M3-docs | Docs follow-ups | M3-17 | CLAUDE.md, docs/plan | Gate docs task. |
| M3-acceptance | Independent test | M3-docs | tests/acceptance/M3/ | Black-box, by agy. |
| M3-dryrun | Dry-run week and evidence | M3-17 | docs/build/reports/ | At least 7 days; report goes to Brendan. |
| M3-golive | Auto at τ | M3-dryrun, M3-acceptance | deploy/ | Only on Brendan's explicit go. Deploy: yes. |

Valid order: M3-0a, M3-0b, M3-1a, M3-1b, M3-1c, M3-2, M3-3, M3-4, M3-5, M3-6, M3-7, M3-8, M3-9, M3-10a, M3-10b, M3-11, M3-12a,
M3-12b, M3-13, M3-14a, M3-14b, M3-15, M3-16, M3-17, M3-docs, M3-acceptance, M3-dryrun, M3-golive.

### What the active plan means for M3
"Trying auto-categorizer" has about 38 transactions in Oct 2026 (roughly 9 a week), 3 linked accounts, many uncategorized rows
and no Groceries category.
- **Labelled data for calibration** (M3-5): approved, categorized transactions from the active plan plus every plan with
  `has_discretionary` true in `plan_scan` (valid per Decisions 2026-10-29), joined to the active plan's categories by exact
  name. There is no remap.
- **Missing categories**: a labelled transaction whose category isn't in the active plan (Groceries, for one) has no correct
  answer at run time. Calibration keeps it as a hard negative: auto-applying it at τ counts as wrong. τ therefore measures the
  real risk of forcing a grocery charge into the nearest category.
- **τ is tied to the list**: adding or renaming a category in the active plan changes the candidate hash, and auto falls back
  to dry-run until `finbot calibrate` reruns. BA-19 says Brendan may still add categories, so expect at least one rerun.
- **Payee history** comes from the same cross-plan labels, because the active plan alone is too thin to feed the rules or Jev.
- **Dry-run week**: about 9 new transactions can't confirm 85% precision (9 of 9 right has a Wilson lower bound near 70%).
  The dry-run also covers the last 45 days of uncategorized backlog, and every decision is a Slack question so that it can be
  judged. Open questions are capped at 5. The report states n and the interval; if fewer than 30 are judged, the go-live choice
  is still Brendan's, made with that number stated.

### Carried follow-ups
| Item | Task |
|---|---|
| Import outcomes only reach logs | M3-15 |
| Import timer resets per deploy | M3-15 (state in M3-1c) |
| `deploy/rollback.sh` has no spike left | M3-0b (retire) |
| ScanDiscretionary omits a failed plan from its list | M3-0a |
| Wrong-PKCE auth code counts in ActiveTokens for 5 min | Deferred to M5: nothing in M3 touches auth/, and it only inflates a count for 5 minutes |
| Unbounded shutdown wait | M3-14a |
| Drop txn.ID from op strings | M3-0a. `ynab/client.go` ops are already fixed strings, so this is a guard test, and M3-7 keeps new ops id-free |
| slack-go v0.32.0 needs Go 1.26 | Verified 2026-11-08: go.mod `go 1.26.0`, Dockerfile `golang:1.26.10-bookworm`, `spikes/slack` builds. M3-10a adds the dependency to the root module |

### Brendan inputs
| Input | First task blocked |
|---|---|
| Confirm the two Slack users (Slack user ids go in `.env` as `SLACK_USERS`; sup can look them up with an OK) and that both are in `#finbot-finances` | M3-14a live use; M3-17 deploy |
| Slack app tidy-up: drop `groups:write`, keep `connections:write`, `chat:write`, `groups:read`, `groups:history`, `users:read`, event `message.groups`, Interactivity on | M3-17 (not a hard block; only the listed scopes are needed) |
| Finish category and card changes in "Trying auto-categorizer" (BA-19) before the calibration that go-live relies on | M3-6 (rerun if the list changes later) |
| OK that a dry-run posts real questions to the shared channel, capped at 5 open | M3-17 |
| Answers to Plan questions 1 to 3 below (defaults are written into the briefs) | M3-11 |
| Go-live after the dry-run report: only Brendan turns on `CLASSIFY_MODE=auto` | M3-golive |
| Whether auto may also write the 45-day backlog, or only new transactions (default: new only) | M3-golive |

Done, no longer inputs: category mapping (as is, 2026-11-08); BA-9 sandbox plan (`YNAB_SANDBOX_PLAN_ID` in the main `.env`).
sup actions, Brendan only if they fail: confirm `JEV_API_KEY` still has budget (M3-6; M0-1b noted a fresh budget may be needed; about $0.07 per 1,000
transactions); copy `CLAUDE_CODE_OAUTH_TOKEN` from hoid's `~/finbot/.env` to the main `.env` unprinted, since deploy.sh builds `finbot.env` from the local file (M3-17).

### Plan questions
1. **Can the claude second opinion cause an auto-apply?** §3.3 calls it a fallback or second opinion on the τ-miss band but gives
   it no threshold, and τ is only measured for Jev. Default in M3-11: advisory only. It's logged in `decisions` and shown in the
   question, never written.
2. **§5 "split" button.** A split needs amounts and M3 has no free text. Default in M3-10b: the button says to split in YNAB and
   closes the question; the next sync locks the row.
3. **Which R11 alerts land in M3?** §3.7 lists all of them, but paycheck and tithing need M4's `profile`. Default: link error, stale
   import, duplicate, price change and large charge in M3; missing or short paycheck and tithing in M4.
4. **§3.3 and R6 say "mapped old→new".** Brendan's 2026-11-08 answer replaces that for M3. M3-docs records it; M4's Fresh Start
   needs a fresh calibration, since the category list and plan change.
5. **Does the active plan have `Discretionary`?** §3.2 sends event spending there and the policy text depends on it, but M3-plan
   had no YNAB access. M3-2 treats a missing category as "policy entry unused" and warns; M3-6 reports which case holds.
6. **Linked-account count.** STATE M2-inv1 found none linked in this plan; the M3-plan brief says 3 (BA-19 came after it). M3-6 and M3-dryrun use the live count.
7. **§3.2 step 4 writes `approved=true`.** An auto-applied row looks identical to a human approval in YNAB. M3-9 tells them apart through write_log, so losing write_log would turn system writes into locks. Backups cover it (N4); no change proposed.

## M4: Budget ownership [expand at gate]

Brendan runs Fresh Start (BA). Covers `seed-plan` (dry-run, approved once), the CSP structure, the resumable paycheck
assignment job, and tithing.

## M5: Writes, intents, audit, reports [expand at gate]

Covers MCP write tools with tiers and the confirmation flow, Slack free text via headless claude, intents, the audit job, and
the monthly check-in plus December nudge.

## v1.1: Amazon enrichment [after v1]
