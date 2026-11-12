# M3-docs: Docs follow-ups

- Milestone: M3
- Branch / worktree: `t/M3-docs`, `../YNAB_AutoCategorizer.wt/M3-docs`
- Depends on: M3-17
- Touches (exclusive while running): CLAUDE.md, docs/plan
- Plan refs: "Docs follow-ups" in each `tasks/M3-*.md` Result; BACKLOG Plan questions
- YNAB access: none
- Deploy: no (smoke: none; docs only)

## Goal
CLAUDE.md and the plan describe what M3 actually built.

## Scope
- Apply every M3 Docs follow-up.
- CLAUDE.md: new packages (`classify/`, `classify/jev/`, `calibrate/`, `llm/`, `writer/`, `slack/`, `alerts/`), subcommands, migrations 0006-0008, config keys, and the safety text. "ImportTransactions is the only YNAB write" changes to name `writer/`. State that `CLASSIFY_MODE=auto` is Brendan's alone.
- architecture.md §3.3 and requirements R6: record the 2026-11-08 decision (M3 calibrates on existing categories, no old→new map; the PROPOSED list goes to M4). Note that M4's Fresh Start needs a fresh calibration.
- Record the outcomes of Plan questions 1 to 3, whatever was decided.
- Remove references to the retired deploy scripts.

## Out of scope
- Behaviour changes. Rewriting history in `docs/build/`.

## Acceptance (each must be demonstrably checked)
- [ ] Every M3 Result's "Docs follow-ups" item is ticked off in this task's Result.
- [ ] `git grep -nE 'rollback\.sh|cutover\.sh' -- CLAUDE.md docs/plan` → no matches.
- [ ] `go run . 2>&1 | head` usage matches the CLAUDE.md subcommand list.
- [ ] No ids, tokens or emails added (grep the diff).
- [ ] go build ./... && go vet ./... && go test ./... green

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->

## Result
- Status: DONE
- What changed:
  - `CLAUDE.md`: Build & Run gains `calibrate`, `dry-run-report`, the Jev/claude/sandbox-write live tests and
    `test-preflight.sh`; the retired cutover/rollback/rehearse scripts and their paragraph are gone (the `tunnel-active`
    marker stays). Docker: pinned `claude` 2.1.312, the compose classify keys, deploy.sh's required/refused keys.
  - `CLAUDE.md` Architecture: new `writer/`, `classify/`, `classify/jev/`, `llm/`, `calibrate/`, `slack/`, `alerts/`
    roles; `cmd/` (import timing + `import_state`, claude probe, categorize after sync with a 5-min bound, shared
    Gate/Writer, `LabelPlans` fail-closed, Slack upsert + self-check, 5 s shutdown deadline), `store/` (0006-0009, new
    tables and read files), `sync/` (lock detection), `ynab/` (`PatchTransactions`), `mcp/` (`get_category_decisions`),
    `status/` (import + classifier lines); key data flow.
  - `CLAUDE.md` Safety/Config: YNAB writes are `ImportTransactions` + `writer/`; `CLASSIFY_MODE=auto` is Brendan's alone;
    new secrets and Slack-id scrubbing; `ANTHROPIC_API_KEY` refused; Slack/Classify/claude config keys; forwarded keys.
  - `docs/plan/requirements.md` R6 and `architecture.md` §3.3: no old→new map in M3 (2026-11-08), PROPOSED list to M4,
    M4's Fresh Start needs a fresh calibration, BA-20 noted. §2 `questions` fields; §3.3/§5/§3.7 record Plan questions
    1-3; §8 Decisions entry; package table `classify/` row.
  - `.env.example`: `SLACK_APP_TOKEN`, `SLACK_USERS` (empty placeholders).
- Docs follow-ups ticked off:
  - [x] M3-0a none. [x] M3-0b CLAUDE.md scripts + cutover paragraph (M2 acceptance files left as historical, per its note).
  - [x] M3-1a, M3-1b, M3-1c migrations/tables/read files; M3-1c §2 `questions` fields. [x] M3-9 0009, sync locks, log counts.
  - [x] M3-2 `classify/` + dropped "returns as classify/ in M3". [x] M3-3 `classify/jev` + JEV_LIVE (`JEV_API_KEY` already
    in `.env.example`). [x] M3-4 `llm/` + CLAUDE_LIVE. [x] M3-5 `calibrate` command + role. [x] M3-6 none.
  - [x] M3-7 `writer/`, `ynab/` + Safety second write. [x] M3-8 sandbox live test line.
  - [x] M3-10a/10b `slack/`. [x] M3-11 pipeline. [x] M3-12a/12b `alerts/`. [x] M3-13 `get_category_decisions`.
  - [x] M3-14a Slack config, serve startup/alerts/shutdown. [x] M3-14b classify keys, data flow, probe, secrets.
  - [x] M3-15 import timing, status lines. [x] M3-16 dry-run-report, `ReportPlans`, fail-closed. [x] M3-17 preflight,
    pinned claude, compose keys, deploy.sh keys. (M3-17's `.env` note is for sup.) [x] M3-plan: CLAUDE.md/docs/plan parts.
  - [x] Plan questions 1-3 recorded (architecture §3.3, §5, §3.7, §8). Plan question 4 recorded (R6, §3.3).
- Acceptance:
  - Follow-ups ticked above → pass.
  - `git grep -nE 'rollback\.sh|cutover\.sh' -- CLAUDE.md docs/plan` → no matches (rc 1) → pass.
  - `go run . 2>&1 | head`: every usage command (serve, sync, health, backup, restore, revoke, tag-account, calibrate,
    dry-run-report, plus `sync --archived`, `backup --list`) is in CLAUDE.md's list → pass.
  - Diff grep for UUIDs, emails, `xox*-`/`xapp-` tokens, Slack user ids, `sk-ant` → none → pass.
  - `go build ./... && go vet ./... && go test ./...` → green.
- Deviations from brief: `.env.example` (outside the touch list) gained `SLACK_APP_TOKEN` and `SLACK_USERS`, since
  CLAUDE.md sends readers there and M3-14a never added them. Empty placeholders only.
- Follow-ups for backlog: none.
- Docs follow-ups: none.
