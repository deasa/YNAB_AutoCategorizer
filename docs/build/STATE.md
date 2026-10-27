# Build state (source of truth — supervisor updates after every event)

Updated: 2026-10-27 by sup (first session)

## Now
- Milestone: **M0 — Spikes** (in progress)
- Next actions: M0-5 merged (architecture impact to bring to Brendan at M0 gate). M0-1 running. Brendan will do BA-2/BA-5 later. Other M0 tasks wait on BA items: M0-2a←BA-5, M0-3←BA-2, M0-4←BA-6 (after install), M0-2b←M0-2a+BA-3+BA-4. Brendan notified 2026-10-27.

## Deployed
- hoid finbot: nothing deployed yet (record SHA + date after each deploy)

## Panes
| Name | Pane | Tab | Role | Task | Started | Notes |
|---|---|---|---|---|---|---|
| sup | w1:pT | control (w1:t7) | supervisor | — | 2026-10-27 | |
| agy | w1:pV | control (w1:t7) | reviewer | — | 2026-10-27 | review counter 1 (M0-5); old over-full agy w1:pS closed |
| b-m0-1 | w1:pY | build (w1:t9) | builder | M0-1 | 2026-10-27 | |
| coordinator | w1:pR | 1 (planning tab) | planning session (not part of the build loop) | — | — | ignore its state changes |

## Tasks
| ID | Status (todo/active/review/fix/merged/blocked) | Pane | Branch | Review | Notes |
|---|---|---|---|---|---|
| M0-1 | active | w1:pY (b-m0-1) | t/M0-1 (../YNAB_AutoCategorizer.wt/M0-1) | | started 2026-10-27 |
| M0-2a | todo (needs BA-5) | | | | |
| M0-2b | todo (needs M0-2a, BA-3, BA-4) | | | | |
| M0-3 | todo (needs BA-2) | | | | |
| M0-4 | todo (needs BA-6) | | | | |
| M0-5 | merged | — | — | agy PASS | merged 2026-10-27; findings docs/build/spikes/ynab.md. M1-1 BLOCKER: ynab v1.6.1 can't decode live plan (`goal_snoozed_at` zone-less). Docs follow-ups in tasks/M0-5.md Result (apply at M0 gate) |

## Brendan actions (BA)
| ID | Action | Needed by | Status |
|---|---|---|---|
| BA-1 | Jev: API key + docs link / account details → put key in main checkout `.env` as `JEV_API_KEY` | M0-1 | done 2026-10-27 (key = Brendan's TYPESAFE_API_KEY, copied to .env by sup; docs via installed `typesafe-ai` skill) |
| BA-2 | Slack: create workspace channel `#finances`; create Slack app (Socket Mode on, bot scopes per M0-3 brief) → `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` in `.env`; invite your wife | M0-3 | open |
| BA-3 | Tell sup the domain served by nginx-proxy-manager; add DNS for `finance.<domain>` | M0-2b | open |
| BA-4 | nginx-proxy-manager: add proxy host `finance.<domain>` → finbot port with Let's Encrypt (or give sup a way to do it) | M0-2b | open |
| BA-5 | Google Cloud: OAuth client (web) with redirect URI from M0-2 brief → `GOOGLE_OAUTH_CLIENT_ID/SECRET`; then add claude.ai custom connector when asked | M0-2a | open |
| BA-6 | On hoid, after M0-4 installs claude: run `claude setup-token` and store as `CLAUDE_CODE_OAUTH_TOKEN` in `~/finbot/.env` | M0-4 | open |
| BA-7 | healthchecks.io (or similar) account → ping URL; connect its alerts to Slack | M1-5 | open |
| BA-8 | Off-site backup target (Backblaze B2 bucket or Google Drive) + credentials; generate age key, keep a copy offline | M1-5 | open |
| BA-10 | YNAB: relink 3 credit-card connections in `direct_import_in_error` (found by M0-5) | M1 sync freshness | open |
| BA-11 | Tell sup whether the old 6h categorizer is running live anywhere (confounds §3.1 payee-memory finding) | M1 | open |
| BA-9 | YNAB: create an empty "finbot-sandbox" plan in the YNAB UI for write tests (API can't create plans) | M3 | open |

## Decisions (made during the build; plan docs hold the earlier ones)
- 2026-10-27: Build workflow approved by Brendan: Opus 5.5 builders, agy review/testing, herdr orchestration, 40% context cap, supervisor restarted from these docs. sup may commit to main.
- 2026-10-27: sup may push origin main and deploy to hoid at its discretion (Brendan).
- 2026-10-27: sup must NOT self-restart; Brendan restarts sup by hand from README.md's bootstrap prompt.
- 2026-10-27: sup may unblock anything in herdr panes in this workspace (Brendan): sup answers permission prompts, trust prompts and approval dialogs for builders and agy itself, using judgment within the plan's safety rules (never move money, no unapproved live-YNAB writes, never print secrets). Escalate only for things needing Brendan's accounts or decisions.

## Milestone log
- (none yet)
