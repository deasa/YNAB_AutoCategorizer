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
