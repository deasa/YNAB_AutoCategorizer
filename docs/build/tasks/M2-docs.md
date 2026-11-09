# M2-docs: apply the M2 docs follow-ups

- Milestone: M2 (gate docs task)
- Branch / worktree: `t/M2-docs`, `../YNAB_AutoCategorizer.wt/M2-docs`
- Depends on: M2-0 .. M2-6 merged (all are)
- Touches (exclusive while running): `CLAUDE.md`, `README.md`, `docs/plan/architecture.md`, `.env.example`
- Plan refs: only the sections the follow-ups name
- YNAB access: none
- Deploy: no

## Goal
CLAUDE.md, README.md, `.env.example` and architecture.md describe main as it is after M2 (MCP + OAuth live behind the
Cloudflare Tunnel, import on in the hoid deploy). This is the one task allowed to edit CLAUDE.md and docs/plan/* for M2.

## Scope
- Apply every item under "Docs follow-ups" in the `## Result` sections of `docs/build/tasks/` M2-0, M2-1a, M2-1b, M2-2,
  M2-3, M2-4, M2-5 and M2-6. Known items, among others:
  - CLAUDE.md package roles for `auth/` and `mcp/`, and the subcommands `finbot revoke` and `finbot tag-account`.
  - Migrations 0004 and 0005 in the `store/` role; `HTTP_ADDR` default `127.0.0.1:8081`.
  - The cutover and rollback scripts and the `tunnel-active` marker, in Build & Run.
  - Backups now hold OAuth state; run `finbot revoke --all` after a restore.
  - `IMPORT_ENABLED` is pinned true in the hoid deploy (M2-6 wording, Build & Run and Safety).
  - architecture.md:48 on plan_scan (M2-0).
  - Old local `.env` files: drop `HTTP_ADDR=:8080` (M2-2).
- README.md: drop the "License: MIT" line (Brendan, BA-14). Add no LICENSE file.
- Check every command, path, env var and default you write against the code on main (grep it). Where a follow-up
  contradicts the code, the code wins: note it under Deviations.

## Out of scope
- Any code, test or deploy change. requirements.md (unless a follow-up names it explicitly).
- New sections or rewrites beyond the follow-ups. Keep CLAUDE.md's style: dense, factual, no ids or secrets.

## Acceptance (each must be demonstrably checked)
- [ ] A table in Result: each follow-up (task, item) → applied at file:line, or skipped with the reason
- [ ] `grep -n "License" README.md` → no "MIT" line
- [ ] Every subcommand in CLAUDE.md's Build & Run exists in `cmd/` dispatch (list them as checked)
- [ ] go build ./... && go vet ./... && go test ./... green (sanity; no code should change)
