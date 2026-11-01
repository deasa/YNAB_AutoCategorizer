# M1-docs: apply M1 docs follow-ups (CLAUDE.md, architecture.md, README)

- Milestone: M1 (gate)
- Branch / worktree: `t/M1-docs`, `../YNAB_AutoCategorizer.wt/M1-docs`
- Depends on: M1-1 … M1-5c (all merged)
- Touches (exclusive while running): `CLAUDE.md`, `docs/plan/architecture.md`, `README.md`. Docs only, no code.
- Plan refs: the "Docs follow-ups" bullets in the `## Result` sections of `docs/build/tasks/M1-4a.md`, `M1-4b.md`,
  `M1-5a.md`, `M1-5b.md`, `M1-5c.md`; `docs/build/STATE.md` → Decisions dated 2026-10-29 and 2026-11-01.
- YNAB access: none
- Deploy: no

## Goal
After this task, CLAUDE.md, architecture.md and README describe finbot as it is now built and deployed at the end of M1,
and the build decisions Brendan made during M1 are recorded in architecture.md.

## Scope
1. **CLAUDE.md:** apply every M1 docs follow-up:
   - package roles for `backup/` and `status/`;
   - the `finbot health`, `backup`, `backup --list` and `restore` commands;
   - the scheduler jobs (sync, import when enabled, backup, status);
   - all configuration: `BACKUP_*`, `HEALTHCHECK_PING_URL`, `SLACK_BOT_TOKEN`, `SLACK_STATUS_CHANNEL`;
   - migrations `0002_plan_scan` and `0003_status_message`;
   - the `cmd.Version` / `GIT_SHA` build arg;
   - the Docker and deploy facts (uid 10001, `./deploy/deploy.sh` ships `git archive HEAD`, `~/finbot/finbot.env`,
     `TZ=America/Denver`);
   - the status update cap, which is 20 s plus a 5 s store write.
   Check every command you document against `./finbot` usage output. Keep it concise.
2. **architecture.md:** small, targeted edits only. Don't restructure.
   - §2/§3.1: the `plan_scan` table; the archived scan result is persisted and read with `store.PlanScans`.
   - §1: `cmd/` holds the subcommands and the scheduler; add the `backup/` and `status/` rows to the package table.
   - §6 N2: the healthchecks.io dead-man ping is replaced by a Slack status message in `#finbot-finances`, edited after
     every sync and backup. Brendan's own Slack automation alerts when it goes stale (decision 2026-11-01). The optional
     ping code remains.
   - §6 N4: the off-site target is Turso DB `finbot-backup`, holding only age-encrypted `VACUUM INTO` snapshots in
     1 MiB chunks, keeping 14, nightly at 03:17 America/Denver. The private key stays off hoid. Restore runs on the Mac.
     §1's "no Turso" (as a primary store) still holds (decision 2026-11-01).
   - §3.1: the live mirror target is the "May 2025" plan until the M4 fresh start (decision 2026-10-29, BA-12). Import
     trigger: built, **off by default** (`IMPORT_ENABLED`). Turning it on live needs Brendan.
3. **README.md:** replace the old-categorizer narrative sections ("How It Works", "Tech Stack", the matching and safety
   sections) with a short description of finbot as built in M1. Point to CLAUDE.md for details.

## Out of scope
- Any code, test or config change. `requirements.md` (no requirement changed). Plans for M2+.

## Acceptance (each must be demonstrably checked)
- [ ] Every "Docs follow-ups" bullet from the five Results is applied, or listed in your Result as "not applied:
      <reason>".
- [ ] Every command and env var named in CLAUDE.md exists: check against `go build -o /tmp/finbot-docs . && /tmp/finbot-docs`
      usage and `grep` of `config/`. Delete the binary afterwards.
- [ ] `git diff --stat main` touches only the three files above.
- [ ] `go build ./... && go test ./...` still green (sanity).

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
