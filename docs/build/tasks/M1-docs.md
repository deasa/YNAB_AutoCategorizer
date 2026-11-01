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

## Result
- Status: DONE
- What changed:
  - `CLAUDE.md`: `health`, `backup`, `backup --list`, `restore`; scheduler jobs; `backup/` and `status/` roles; migrations through `0003_status_message`; all optional config; `cmd.Version`/`GIT_SHA`; Docker and deploy facts; the 20 s + 5 s status cap.
  - `docs/plan/architecture.md`: §1 rows for `cmd/`, `backup/`, `status/`; §2 `plan_scan`; §3.1 live mirror target, persisted scan, import off by default; §6 N2 and N4 per the 2026-11-01 decision.
  - `README.md`: the old-categorizer narrative is replaced by a short description of finbot as built in M1, pointing to CLAUDE.md.
- Acceptance:
  - Every "Docs follow-ups" bullet from M1-4a, 4b, 5a, 5b, 5c applied → pass (read against each Result; none left unapplied). M1-4a's and M1-4b's CLAUDE.md items were already in place from M1-4b.
  - Commands → pass: `go build -o /tmp/finbot-docs . && /tmp/finbot-docs` lists serve, sync, sync --archived, health, backup, backup --list, restore, matching CLAUDE.md. Binary deleted.
  - Env vars → pass: each name in CLAUDE.md grepped in `config/config.go` and `.env.example`. The four that aren't there exist elsewhere: `GIT_SHA` (Dockerfile build arg), `YNAB_LIVE_SMOKE`, `YNAB_LIVE_SYNC`, `YNAB_LIVE_SYNC_LOG` (test gates, grepped in `*_test.go`).
  - Diff scope → pass with a note: `git diff --stat main...HEAD` shows only the three files plus this brief's Result. Two-dot `git diff --stat main` also lists `docs/build/STATE.md`, because main's STATE.md moved after this branch was cut; this branch doesn't touch it.
  - `go build ./... && go vet ./... && go test ./...` → pass.
- Deviations from brief:
  - The README rewrite goes beyond the named sections: Configuration, Running, Project Structure and YNAB API Notes are dropped too, since CLAUDE.md covers them. The Configuration, Running and Project Structure sections were accurate but incomplete; YNAB API Notes still described the categorizer's API use.
  - CLAUDE.md Safety also names the new secrets (ping URL, Turso URL and token, Slack token), which the brief didn't list.
- Follow-ups for backlog: none
- Docs follow-ups (found here, outside this brief's list):
  - Applied on request: architecture.md §6 N3 now says the deploy ships `git archive HEAD`, not rsync; §9 marks U10 resolved by the 2026-11-01 Slack status message decision.
  - Not changed, on request: the README keeps "License: MIT", but the repo has no LICENSE file.

### Fix round 1
Review: `../_reviews/M1-docs.md`. Findings 1-8 are applied. Each was checked against the code first where it makes a code claim.
1. architecture.md §6 N3: `finbot.env` now lists the age recipient (public) and Turso credentials, never the backup private key.
2. architecture.md §2: `plan_scan` moved below the split-parent sub-bullet.
3. architecture.md §7: the M1 line says "Slack status heartbeat".
4. CLAUDE.md `cmd/`: a job's own tick is dropped while it runs; other jobs' ticks run right after (`cmd/scheduler.go`).
5. "Scheduled sync and backup" in CLAUDE.md, README and architecture.md (§1 `status/` row and §6 N2). The same limit applies to the ping (`cmd/app.go` `newScheduler`), so CLAUDE.md's two ping mentions say "scheduled" too.
6. CLAUDE.md `backup/`: "has no mirror rows at all" (`checkSnapshot`).
7. CLAUDE.md Configuration: notes the `finbot health` and `deploy.sh` exceptions.
8. CLAUDE.md Safety: warns about `serve` with the real `.env`, and says to blank `SLACK_BOT_TOKEN` and `BACKUP_*` for local runs. Code read only; not run.
9. Not applied: the license is left for Brendan.

Re-checks:
- Commands → pass: the built binary's usage lists all 7 documented forms. Binary deleted.
- Env vars → pass: every name in CLAUDE.md is in `config/config.go` and `.env.example`, except `GIT_SHA` (Dockerfile, compose, `deploy.sh`) and the three `YNAB_LIVE_*` test gates. The `deploy.sh` forward list matches the new Configuration note.
- Diff scope → pass: `git diff --stat main...HEAD` shows the three files plus this brief.
- `go build ./... && go vet ./... && go test ./...` → pass.
