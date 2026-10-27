# Build state (source of truth — supervisor updates after every event)

Updated: 2026-10-27 by planning coordinator, before the first sup started

## Now
- Milestone: **M0 — Spikes** (not started)
- Next actions: Brendan completes BA items below; sup dispatches M0-3 and M0-5 immediately (no BA dependency except Slack for M0-3), then others as BAs land.

## Deployed
- hoid finbot: nothing deployed yet (record SHA + date after each deploy)

## Panes
| Name | Pane | Tab | Role | Task | Started | Notes |
|---|---|---|---|---|---|---|
| agy | w1:pS | 1 (planning tab) | reviewer | — | — | context already large (~147k tokens from planning): REPLACE with a fresh agy in tab `control` before its first review; then counter 0 |
| coordinator | w1:pR | 1 (planning tab) | planning session (not part of the build loop) | — | — | ignore its state changes |

## Tasks
| ID | Status (todo/active/review/fix/merged/blocked) | Pane | Branch | Review | Notes |
|---|---|---|---|---|---|
| M0-1 | todo (needs BA-1) | | | | |
| M0-2a | todo (needs BA-5) | | | | |
| M0-2b | todo (needs M0-2a, BA-3, BA-4) | | | | |
| M0-3 | todo (needs BA-2) | | | | |
| M0-4 | todo (needs BA-6) | | | | |
| M0-5 | todo | | | | |

## Brendan actions (BA)
| ID | Action | Needed by | Status |
|---|---|---|---|
| BA-1 | Jev: API key + docs link / account details → put key in main checkout `.env` as `JEV_API_KEY` | M0-1 | open |
| BA-2 | Slack: create workspace channel `#finances`; create Slack app (Socket Mode on, bot scopes per M0-3 brief) → `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` in `.env`; invite your wife | M0-3 | open |
| BA-3 | Tell sup the domain served by nginx-proxy-manager; add DNS for `finance.<domain>` | M0-2b | open |
| BA-4 | nginx-proxy-manager: add proxy host `finance.<domain>` → finbot port with Let's Encrypt (or give sup a way to do it) | M0-2b | open |
| BA-5 | Google Cloud: OAuth client (web) with redirect URI from M0-2 brief → `GOOGLE_OAUTH_CLIENT_ID/SECRET`; then add claude.ai custom connector when asked | M0-2a | open |
| BA-6 | On hoid, after M0-4 installs claude: run `claude setup-token` and store as `CLAUDE_CODE_OAUTH_TOKEN` in `~/finbot/.env` | M0-4 | open |
| BA-7 | healthchecks.io (or similar) account → ping URL; connect its alerts to Slack | M1-5 | open |
| BA-8 | Off-site backup target (Backblaze B2 bucket or Google Drive) + credentials; generate age key, keep a copy offline | M1-5 | open |
| BA-9 | YNAB: create an empty "finbot-sandbox" plan in the YNAB UI for write tests (API can't create plans) | M3 | open |

## Decisions (made during the build; plan docs hold the earlier ones)
- 2026-10-27: Build workflow approved by Brendan: Opus 5.5 builders, agy review/testing, herdr orchestration, 40% context cap, supervisor restarted from these docs. sup may commit to main.
- 2026-10-27: sup may push origin main and deploy to hoid at its discretion (Brendan).
- 2026-10-27: sup must NOT self-restart; Brendan restarts sup by hand from README.md's bootstrap prompt.
- 2026-10-27: sup may unblock anything in herdr panes in this workspace (Brendan): sup answers permission prompts, trust prompts and approval dialogs for builders and agy itself, using judgment within the plan's safety rules (never move money, no unapproved live-YNAB writes, never print secrets). Escalate only for things needing Brendan's accounts or decisions.

## Milestone log
- (none yet)
