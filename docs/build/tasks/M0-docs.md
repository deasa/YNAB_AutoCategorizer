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
