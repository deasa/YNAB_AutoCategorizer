# Build state (source of truth — supervisor updates after every event)

Updated: 2026-10-29 by sup (first session)

## Now
- Milestone: **M0 — Spikes** (in progress)
- Next actions: M0-5 merged (architecture impact to bring to Brendan at M0 gate). M0-1 merged. M0-1b merged. Brendan plans to dramatically simplify YNAB categories (input: spikes/jev.md per-category section). BA-2 done; M0-3 merged. M0-4 split: M0-4a and M0-4b merged. BA-5 done 2026-10-29; M0-2a merged 2026-10-29. **Only M0-2b remains**: brief written for Cloudflare Tunnel (decision 2026-10-29); dispatch once Brendan confirms the tunnel public hostname + Google redirect URI. Then the M0 gate. sup loop paused until then. Other M0 tasks wait on BA items: M0-2a←BA-5, M0-4←BA-6 (after install), M0-2b←M0-2a+BA-3+BA-4. Brendan notified 2026-10-27.

## Deployed
- hoid finbot: nothing deployed yet (record SHA + date after each deploy)

## Panes
| Name | Pane | Tab | Role | Task | Started | Notes |
|---|---|---|---|---|---|---|
| sup | w1:pT | control (w1:t7) | supervisor | — | 2026-10-27 | |
| agy | w1:p17 | control (w1:t7) | reviewer | — | 2026-10-29 | review counter 1 (M0-2a r3) |
| coordinator | w1:pR | 1 (planning tab) | planning session (not part of the build loop) | — | — | ignore its state changes |

## Tasks
| ID | Status (todo/active/review/fix/merged/blocked) | Pane | Branch | Review | Notes |
|---|---|---|---|---|---|
| M0-1 | merged | — | — | agy PASS (1 low, optional) | merged 2026-10-27; findings docs/build/spikes/jev.md: Jev primary behind rules, τ=0.8 → ~90% precision / ~73% coverage. Docs follow-ups in tasks/M0-1.md Result. Note: no YNAB approvals since 2026-04-20 |
| M0-1b | merged | — | — | agy PASS (1 low wording nit) | merged 2026-10-27; per-category difficulty in spikes/jev.md for Brendan's category simplification. Follow-up: re-measure Jev after simplification (needs a fresh Jev budget) |
| M0-2a | merged | — | — | agy PASS r3 (r1 FIX, r2 FIX; 1 low) | merged 2026-10-29; findings docs/build/spikes/mcp.md, reviews docs/build/spikes/mcp-review.md. go-sdk v1.8.0 + fosite v0.49.0 AS (DCR, PKCE S256, opaque HMAC tokens, refresh rotation, RFC 7009) + Google sign-in via go-oidc, allowlist on verified email, CSRF consent page, per-user generation revocation; spike module go 1.26. Brendan live sign-in OK twice (incl. consent page). **M0-2b MUST**: proxy to the container on the Docker network with `MCP_LISTEN_ADDR=:8080` and no host port, never via host 127.0.0.1 (go-sdk loopback DNS-rebinding guard → 403 on `finance.<domain>` Host); add `https://finance.<domain>/oauth/google/callback` to the Google client; persist the fosite HMAC secret, or accept re-login per deploy. Other follow-ups in tasks/M0-2a.md Result |
| M0-2b | todo (needs M0-2a, BA-3, BA-4) | | | | |
| M0-3 | merged | — | — | agy PASS (2 low, for M3) | merged 2026-10-28; findings docs/build/spikes/slack.md. M3 notes: extend existing `questions` table (§2) rather than new `slack_questions`; slack-go v0.32.0 needs Go 1.26 → bump go.mod + Dockerfile (or pin older slack-go). Slack app tidy-up (drop `groups:write`, maybe `channels:*`) at M0 gate |
| M0-4a | merged | — | — | agy PASS (4 low) | merged 2026-10-28; claude 2.1.312 (latest channel) at hoid ~/.local/bin; findings docs/build/spikes/claude-cli.md. agy findings (docs/build/spikes/claude-cli-review.md) feed M0-4b + Dockerfile: pass prompt via stdin/`--` (variadic --tools eats positional prompt); check permission-mode can't auto-approve MCP tools outside --allowedTools; disable auto-memory + CLAUDE.md discovery; Docker: debian-slim or Alpine musl deps, CLAUDE_CONFIG_DIR persistent |
| M0-4b | merged | — | — | agy PASS (2 low; reproduced the dontAsk finding) | merged 2026-10-28; final §5a command lines in docs/build/spikes/claude-cli.md. MUST for M3/M5 `llm/`: `--permission-mode dontAsk` on the Slack site (auto executed a tool outside --allowedTools); `--system-prompt-snapshot off` with --resume; stdin prompts; `--mcp-config` as a 0600 temp file (bearer token never on argv); test HTTP MCP + Authorization header across --resume; health check `rc==0 && !is_error && trimmed lower result=="ok"`; classifier treats null structured_output as skip. Review kept at docs/build/spikes/claude-cli-review-4b.md |
| M0-5 | merged | — | — | agy PASS | merged 2026-10-27; findings docs/build/spikes/ynab.md. M1-1 BLOCKER: ynab v1.6.1 can't decode live plan (`goal_snoozed_at` zone-less). Docs follow-ups in tasks/M0-5.md Result (apply at M0 gate) |

## Brendan actions (BA)
| ID | Action | Needed by | Status |
|---|---|---|---|
| BA-1 | Jev: API key + docs link / account details → put key in main checkout `.env` as `JEV_API_KEY` | M0-1 | done 2026-10-27 (key = Brendan's TYPESAFE_API_KEY, copied to .env by sup; docs via installed `typesafe-ai` skill) |
| BA-2 | Slack: create workspace channel `#finances`; create Slack app (Socket Mode on, bot scopes per M0-3 brief) → `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` in `.env`; invite your wife | M0-3 | done 2026-10-28: app finbot (Ashton Clan, 16 humans), private `#finbot-finances`, tokens verified by sup; `message.groups` event added 2026-10-28 |
| BA-3 | ~~Tell sup the domain served by nginx-proxy-manager; add DNS~~ → redefined 2026-10-29: move `hoidlabs.site` DNS to Cloudflare (free) | M0-2b | done 2026-10-29 (nameservers switched to Cloudflare; DNSSEC was off; old A records to home IP deleted) |
| BA-4 | ~~nginx-proxy-manager proxy host~~ → redefined 2026-10-29: Cloudflare Tunnel `hoid`, token in hoid `~/finbot/.env` as `CLOUDFLARE_TUNNEL_TOKEN`; public hostname `finance.hoidlabs.site` → `HTTP finbot:8080`; add `https://finance.hoidlabs.site/oauth/google/callback` to the Google client | M0-2b | token done 2026-10-29 (verified shape, perms 600); public hostname + Google redirect URI pending confirmation |
| BA-5 | Google Cloud: OAuth client (web) with redirect URI from M0-2 brief → `GOOGLE_OAUTH_CLIENT_ID/SECRET`; then add claude.ai custom connector when asked | M0-2a | done 2026-10-29 (project finbot, consent External/Testing with 2 test users, redirect `http://localhost:8080/oauth/google/callback`; sup fixed key-name typo, set `.env` 600, added `MCP_ALLOWED_EMAILS` = Brendan only). Still to do: add `https://finance.<domain>/oauth/google/callback` (M0-2b), claude.ai connector when asked |
| BA-6 | On hoid: run `DISABLE_UPDATES=1 claude setup-token` (keeps hoid on 2.1.312) and store as `CLAUDE_CODE_OAUTH_TOKEN` in `~/finbot/.env` | M0-4b | done 2026-10-28 (3rd token; perms 600; sup auth probe OK on claude-haiku-5-5). Note: sup accidentally printed ~29 trailing chars of the 1st token while masking a malformed .env; that token was replaced |
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

- 2026-10-29: Public exposure = **Cloudflare Tunnel** (Brendan, on sup's recommendation), replacing Q16's nginx-proxy-manager assumption. Found: domain is `hoidlabs.site` (Squarespace Domains, exp. 2028), served by hoid's *system* nginx with certbot; NPM installed but unused; router does not forward 80/443 (ECONNREFUSED from outside); home IP fixedness unknown. Tunnel needs no port forwarding, no sudo, works with a dynamic IP/CGNAT, hides the home IP. cloudflared runs in the finbot compose project and reaches `finbot:8080` over the compose network. Plan docs (§4 exposure, rate-limit/fail2ban → Cloudflare rules) updated at the M0 gate.

## Milestone log
- (none yet)
