# Build state (source of truth — supervisor updates after every event)

Updated: 2026-10-27 by sup (first session)

## Now
- Milestone: **M0 — Spikes** (in progress)
- Next actions: M0-5 merged (architecture impact to bring to Brendan at M0 gate). M0-1 merged. M0-1b merged. Brendan plans to dramatically simplify YNAB categories (input: spikes/jev.md per-category section). BA-2 done; M0-3 merged. M0-4 split: M0-4a (install on hoid, Brendan approved 2026-10-28) running; M0-4b (runtime flag checks) waits on BA-6. Brendan will do BA-5 later. Other M0 tasks wait on BA items: M0-2a←BA-5, M0-4←BA-6 (after install), M0-2b←M0-2a+BA-3+BA-4. Brendan notified 2026-10-27.

## Deployed
- hoid finbot: nothing deployed yet (record SHA + date after each deploy)

## Panes
| Name | Pane | Tab | Role | Task | Started | Notes |
|---|---|---|---|---|---|---|
| sup | w1:pT | control (w1:t7) | supervisor | — | 2026-10-27 | |
| agy | w1:p0 | control (w1:t7) | reviewer | — | 2026-10-27 | review counter 2 (M0-1b, M0-3): replace before next review |
| b-m0-4a | w1:p12 | build | builder | M0-4a | 2026-10-28 | |
| coordinator | w1:pR | 1 (planning tab) | planning session (not part of the build loop) | — | — | ignore its state changes |

## Tasks
| ID | Status (todo/active/review/fix/merged/blocked) | Pane | Branch | Review | Notes |
|---|---|---|---|---|---|
| M0-1 | merged | — | — | agy PASS (1 low, optional) | merged 2026-10-27; findings docs/build/spikes/jev.md: Jev primary behind rules, τ=0.8 → ~90% precision / ~73% coverage. Docs follow-ups in tasks/M0-1.md Result. Note: no YNAB approvals since 2026-04-20 |
| M0-1b | merged | — | — | agy PASS (1 low wording nit) | merged 2026-10-27; per-category difficulty in spikes/jev.md for Brendan's category simplification. Follow-up: re-measure Jev after simplification (needs a fresh Jev budget) |
| M0-2a | todo (needs BA-5) | | | | |
| M0-2b | todo (needs M0-2a, BA-3, BA-4) | | | | |
| M0-3 | merged | — | — | agy PASS (2 low, for M3) | merged 2026-10-28; findings docs/build/spikes/slack.md. M3 notes: extend existing `questions` table (§2) rather than new `slack_questions`; slack-go v0.32.0 needs Go 1.26 → bump go.mod + Dockerfile (or pin older slack-go). Slack app tidy-up (drop `groups:write`, maybe `channels:*`) at M0 gate |
| M0-4a | active | w1:p12 (b-m0-4a) | t/M0-4a (../YNAB_AutoCategorizer.wt/M0-4a) | | started 2026-10-28; install native claude on hoid (~/.local/bin, no sudo), offline flag check (U11/U12 research dropped 2026-10-28, Brendan settled them) |
| M0-4b | todo (needs M0-4a, BA-6) | | | | runtime checks of §5a flags with CLAUDE_CODE_OAUTH_TOKEN |
| M0-5 | merged | — | — | agy PASS | merged 2026-10-27; findings docs/build/spikes/ynab.md. M1-1 BLOCKER: ynab v1.6.1 can't decode live plan (`goal_snoozed_at` zone-less). Docs follow-ups in tasks/M0-5.md Result (apply at M0 gate) |

## Brendan actions (BA)
| ID | Action | Needed by | Status |
|---|---|---|---|
| BA-1 | Jev: API key + docs link / account details → put key in main checkout `.env` as `JEV_API_KEY` | M0-1 | done 2026-10-27 (key = Brendan's TYPESAFE_API_KEY, copied to .env by sup; docs via installed `typesafe-ai` skill) |
| BA-2 | Slack: create workspace channel `#finances`; create Slack app (Socket Mode on, bot scopes per M0-3 brief) → `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` in `.env`; invite your wife | M0-3 | done 2026-10-28: app finbot (Ashton Clan, 16 humans), private `#finbot-finances`, tokens verified by sup; `message.groups` event added 2026-10-28 |
| BA-3 | Tell sup the domain served by nginx-proxy-manager; add DNS for `finance.<domain>` | M0-2b | open |
| BA-4 | nginx-proxy-manager: add proxy host `finance.<domain>` → finbot port with Let's Encrypt (or give sup a way to do it) | M0-2b | open |
| BA-5 | Google Cloud: OAuth client (web) with redirect URI from M0-2 brief → `GOOGLE_OAUTH_CLIENT_ID/SECRET`; then add claude.ai custom connector when asked | M0-2a | open |
| BA-6 | On hoid, after M0-4a installs claude: run `claude setup-token` and store as `CLAUDE_CODE_OAUTH_TOKEN` in `~/finbot/.env` | M0-4 | open |
| BA-7 | healthchecks.io (or similar) account → ping URL; connect its alerts to Slack | M1-5 | open |
| BA-8 | Off-site backup target (Backblaze B2 bucket or Google Drive) + credentials; generate age key, keep a copy offline | M1-5 | open |
| BA-10 | YNAB: relink 3 credit-card connections in `direct_import_in_error` (found by M0-5) | M1 sync freshness | open |
| BA-11 | Tell sup whether the old 6h categorizer is running live anywhere (confounds §3.1 payee-memory finding) | M1 | done 2026-10-27: Brendan believes it is not running → M0-5's 9/75 unapproved+categorized imports are likely YNAB payee memory (§3.1 assumption leans VERIFIED; still confirm by observation in M1) |
| BA-9 | YNAB: create an empty "finbot-sandbox" plan in the YNAB UI for write tests (API can't create plans) | M3 | open |

## Decisions (made during the build; plan docs hold the earlier ones)
- 2026-10-27: Build workflow approved by Brendan: Opus 5.5 builders, agy review/testing, herdr orchestration, 40% context cap, supervisor restarted from these docs. sup may commit to main.
- 2026-10-27: sup may push origin main and deploy to hoid at its discretion (Brendan).
- 2026-10-27: sup must NOT self-restart; Brendan restarts sup by hand from README.md's bootstrap prompt.
- 2026-10-27: sup may unblock anything in herdr panes in this workspace (Brendan): sup answers permission prompts, trust prompts and approval dialogs for builders and agy itself, using judgment within the plan's safety rules (never move money, no unapproved live-YNAB writes, never print secrets). Escalate only for things needing Brendan's accounts or decisions.
- 2026-10-27: Category simplification (Brendan): purpose/event categories (Birthday, Trip, Christmas, Giving, etc.) become **savings set-asides only**. Their job is to accumulate enough money to afford the event; spending is NOT tracked per event, and after the event the set-aside balance is zeroed out. Consequences: these categories are never classifier targets (manual-only/excluded, like Birthday/Gift today); M3 old→new category mapping and M4 budget structure (CSP savings goals) follow this. Confirmed by Brendan: event spending is categorized **Discretionary**; zeroing a set-aside is **never automatic**: Brendan does it by hand in YNAB or asks an AI to do it through the MCP (a user-initiated M5 write tool, under the normal confirmation flow). Plan docs to update at M0 gate.
- 2026-10-28: U11 and U12 resolved by Brendan: subscription capacity is sufficient for finbot's headless `claude` use, and his use is within the approved terms. No further research.
- 2026-10-28: Headless `claude` on hoid uses **Haiku** (Brendan); applies to both §5a call sites unless a later decision says otherwise. Update architecture.md §5a at the M0 gate.

## Milestone log
- (none yet)
