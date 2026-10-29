# M0-docs: apply M0 findings and decisions to the plan docs

- Milestone: M0 (gate)
- Branch / worktree: `t/M0-docs`, `../YNAB_AutoCategorizer.wt/M0-docs`
- Depends on: all M0 tasks (merged)
- Touches (exclusive while running): `docs/plan/architecture.md`, `docs/plan/requirements.md` (only where it lists the
  U-items or contradicts a decision), `docs/build/BACKLOG.md` (M1 notes and the M2+ sections only),
  `docs/build/spikes/claude-cli.md`, `docs/build/spikes/mcp.md`
- Plan refs: everything listed below. You are editing the plan, so keep it the plan's voice: concise, decisions not
  history
- YNAB access: none
- Deploy: no

## Goal
`architecture.md` reflects what M0 proved and what Brendan decided, so M1+ builders can trust it without reading five spike
docs. Every "Docs follow-ups" item from M0 is applied once, and nothing else changes.

## Inputs (read in this order)
1. `docs/build/spikes/M0-acceptance.md`: agy's per-section update map. This is your checklist.
2. `docs/build/STATE.md`: the **Decisions** section (every entry dated 2026-10-27..29) and the M0 task-row notes.
3. The "Docs follow-ups" bullet in each Result: `docs/build/tasks/M0-{5,1,1b,3,4a,4b,2a,2b}.md`.
4. The spike docs, only to check a detail: `docs/build/spikes/{ynab,jev,slack,claude-cli,mcp}.md`.

## Scope
- **`architecture.md`:** update §0–§9 per the map. Make sure these are in:
  - Go 1.26;
  - a glibc (Debian-slim) runtime image;
  - Cloudflare Tunnel with a cloudflared sidecar instead of nginx-proxy-manager, and Cloudflare rate-limit rules instead of
    fail2ban (block-only, never a challenge, spare Anthropic's egress range);
  - split 0600 env files;
  - the go-sdk/fosite/go-oidc auth design, with persistent fosite storage in **M2**;
  - the Jev primary classifier (batch-first `classify.Model`, payee history + policy, τ starts at 0.8);
  - the category simplification (event set-asides are savings only, event spending → Discretionary, zeroing never
    automatic);
  - the PROPOSED-categories rule (2026-10-29 decision);
  - older plans containing "Discretionary" are valid labelled data;
  - Slack details (`message.groups`, extend `questions` rather than a new table);
  - the §5a flag set (Haiku, `dontAsk`, stdin prompts, 0600 temp `--mcp-config`, `--system-prompt-snapshot off`, the
    isolation env vars, pinned 2.1.312, the health probe);
  - the YNAB client notes (zone-shim, redaction, local 200/hr counter, delta-only tombstones);
  - Q15–Q17 updated;
  - U1, U8, U11, U12 closed and U9 narrowed (Claude verified; ChatGPT and Gemini unverified).
- **`BACKLOG.md`:**
  - add notes to the M1 rows where M0 changed them: M1-1 Go 1.26 and the zone shim, M1-5a Debian image and split env
    files. Don't rename or re-scope tasks.
  - in the M2 section, add fosite SQLite storage and a persisted HMAC secret, Cloudflare rate-limit rules, container
    hardening (agy M0-2b notes), and the wife's real sign-in.
  - don't touch the M1 rows' IDs or the order.
- **Spike doc cleanups** (agy finding 1):
  - add VERIFIED/UNVERIFIED labels to `claude-cli.md`'s M0-4a half, fix its stale header and the `!.is_error` mismatch;
  - mark `mcp.md` §2/§6/§10 NPM text as superseded by §11, with a one-line note at each.
- **No changes to:** `CLAUDE.md` (it describes the current code; M1-4 updates it), `docs/build/{README,SUPERVISOR,
  WORKER,REVIEWER,STATE}.md`, or any code.

## Out of scope
- New design: if the inputs conflict, or a decision is missing, signal a QUESTION. Don't invent one.
- Rewriting sections that M0 didn't affect.

## Acceptance (each must be demonstrably checked)
- [ ] Every line of the M0-acceptance.md update map is applied. Your Result has a checklist mapping each map bullet to
      the section edited.
- [ ] Every "Docs follow-ups" bullet from the 8 M0 Results is applied or explicitly marked "already covered / not
      applicable" with a reason.
- [ ] `git diff main...HEAD --stat` touches only the files listed in Touches.
- [ ] No secrets, emails, ids or tunnel ids in the diff (grep it and show the check).

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->

## Result
- Status: DONE
- What changed:
  - `docs/plan/architecture.md`: §0–§9 updated from the M0 spikes and the 2026-10-27..29 decisions (Status line notes the M0-gate update).
  - `docs/plan/requirements.md`: U1 moved to resolved (the only U-item it lists).
  - `docs/build/BACKLOG.md`: M0 notes on M1-1 and M1-5a (IDs/order/scope unchanged); M0 notes block in the M2 section.
  - `docs/build/spikes/claude-cli.md`: header refreshed, VERIFIED/UNVERIFIED labels on the M0-4a half, `!.is_error` fixed in the health-check line.
  - `docs/build/spikes/mcp.md`: one-line "superseded by §11" notes at §2, §6, §10 (the §10 note also records M3 → M2 for `mcp/` + `auth/`).
- Acceptance:
  - M0-acceptance.md map → section edited (all pass):
    - §0 Go 1.26; no 80/443 forwarding, NPM unused → §0
    - §1 `mcp/` + `auth/`, `finbot revoke`, cloudflared sidecar (no host ports), glibc image → §1 (command list, deploy paragraph, package table)
    - §2 split-parent sentinel; `slack_questions` folded into `questions`; `users` slack_user_id + `u_<sha256(email)[:12]>`; fosite + `token_generations` in M2 → §2
    - §3.1 zone-less fix, plan-id redaction, local 200/hr counter, delta-only tombstones vs full-export deleted categories, Inflow: RTA skip, link-error dedupe, freshness via `max(date) WHERE import_id IS NOT NULL` + `synced_at` → §3.1 steps 1–4 + client notes; `ynab/` row in §1
    - §3.2–§3.3 batch-first `classify.Model`, `jev-1.14.0` + payee history + `Discretionary` policy, τ=0.8, P2P detector, failed/null → skip, event categories excluded → §3.2 steps 1–5 + category-targets bullet, §3.3
    - §3.5 & §4 set-asides never auto-zeroed (M5 tool only); go-sdk v1.8.0 + fosite v0.49.0 + go-oidc (DCR, S256, CSRF consent, opaque HMAC, generation revoke); Cloudflare Tunnel; block-only rate limits skipping `160.79.104.0/21` → §3.5, §4
    - §5 & §5a slack-go v0.32.0, `message.groups`, drop `groups:write`, ack-first, ignore `bot_id`, idempotency, startup channel check; pin 2.1.312, haiku, stdin, `DISABLE_UPDATES=1`, isolation env vars, `--setting-sources ""`, `dontAsk`, 0600 temp `--mcp-config`, snapshot off, `--no-session-persistence`, `rc==0 && !.is_error` probe → §5, §5a; token-scoped `tools/list` → §4
    - §6–§9 split 0600 `finbot.env`/`tunnel.env` + container/log hardening; fosite SQLite in M2; Q15–Q17; U1/U8/U11/U12 closed, U9 narrowed → §6, §7 (M0/M1/M2 lines), §8, §9
  - Brief-specific items also in: PROPOSED-categories rule (§3.3, §8), older `Discretionary` plans = valid labelled data (§3.3), Debian-slim (§1).
  - Docs follow-ups from the 8 M0 Results:
    - M0-5: §2 sentinel/FK → §2; §3.1 step 4 import timestamp → §3.1; §9 delta/tombstone/link resolved, payee memory still open → §3.1 step 2 + §9; CLAUDE.md quirk note → not applicable here (CLAUDE.md is out of scope; M1-1/M1-4 own it).
    - M0-1: U1 → §9 + requirements.md; §3.2 step 3 policy override → §3.2; batch-first `classify.Model` → §3.3.
    - M0-1b: none.
    - M0-3: `message.groups`, scopes, bot-message ignore, id-keyed users → §5/§2; `slack_questions` → folded into `questions` per the 2026-10-29 sup decision (agy finding 2); §6 Slack event-subscription checklist → §6.
    - M0-4a: haiku, hidden `--append-system-prompt-file`, `DISABLE_UPDATES=1`, `~/.claude.json` on volume, pin 2.1.312 → §5a; U11/U12 → §9; filename rename → already done (not a doc change).
    - M0-4b: `dontAsk`, haiku, env vars, stdin, literal `--setting-sources ""`, resume + new `--mcp-config` resolved, snapshot off, latency → §5a.
    - M0-2a: fosite AS + go-sdk RS helpers, v1.8.0 → §4; U8 closed, U9 → §9; CLAUDE.md/config env vars → not applicable (CLAUDE.md out of scope; config lands with M2 code).
    - M0-2b: Cloudflare Tunnel sidecar, rate-limit rules + in-app caps, `CF-Connecting-IP` rule → §4; mcp.md §10 outdated → note added; U9 → §4 + §9.
  - `git diff main...HEAD --stat` → only BACKLOG.md, spikes/claude-cli.md, spikes/mcp.md, plan/architecture.md, plan/requirements.md: pass.
  - Secrets grep over added lines (`git diff main...HEAD | grep '^+' | grep -niE 'emails|gmail|sk-ant|xox*|xapp-|JWT|UUIDs|32+ hex|TUNNEL_TOKEN=|CLIENT_SECRET=|googleusercontent'`) → no matches: pass. Only the public hostname `finance.hoidlabs.site` (already in STATE.md) appears.
  - `go build ./... && go vet ./... && go test ./...` → pass (no code changed).
- Deviations from brief: none. Small judgement calls: deploy-script hardening (agy M0-2b finding 1, "M1's production deploy script") went into the M1-5a note alongside the Debian/split-env note; mcp.md's §9/§11 "M3" mentions are corrected by the §10 note rather than edited in place.
- Follow-ups for backlog: none new (M3 items already flow through architecture §2/§3.2/§5).
- Docs follow-ups: CLAUDE.md (M1-4): ynab v1.6.1 zone-less `goal_snoozed_at` transport, Go 1.26; config docs for `MCP_PUBLIC_URL`, `MCP_LISTEN_ADDR`, `MCP_DATA_DIR`, `MCP_ALLOWED_EMAILS`, Google OAuth vars when M2 lands.
