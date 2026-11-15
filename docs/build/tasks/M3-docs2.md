# M3-docs2: Docs follow-ups (late M3 tasks)

- Milestone: M3
- Branch / worktree: `t/M3-docs2`, `../YNAB_AutoCategorizer.wt/M3-docs2`
- Depends on: M3-acceptance, M3-other, M3-policy, M3-cats (all merged)
- Touches (exclusive while running): CLAUDE.md
- Plan refs: the "Docs follow-ups" lines in the Result sections of `tasks/M3-acceptance.md`, `tasks/M3-other.md`,
  `tasks/M3-policy.md`, `tasks/M3-cats.md`
- YNAB access: none
- Deploy: no (docs only)

## Goal
CLAUDE.md describes the four M3 tasks merged after M3-docs.

## Scope
- M3-acceptance: in "Env-gated live tests", add `tests/acceptance/M3/sandbox_check.sh` (sandbox writes) and
  `tests/acceptance/M3/hoid_check.sh` (read-only). Check each script's header for how it is run.
- M3-other: rewrite the `slack/` bullet's question sentence per the follow-up (other… always, the `classify.Eligible`
  select, loose thread-reply matching, hint line).
- M3-policy: the `classify/` bullet's policy wording per the follow-up.
- M3-cats: add `./finbot restructure ...` to Build & Run with its real flags (check `go run . restructure -h` or the
  usage text), one short comment saying it is manual and `--apply` on the live plan needs `--confirm-live` and Brendan.
  The `ynab/` and Safety text already name it; don't duplicate.

## Out of scope
- Behaviour changes, `docs/plan/*`, `docs/build/*` except this brief's Result.

## Acceptance (each must be demonstrably checked)
- [ ] Each of the four follow-ups is ticked off in this task's Result.
- [ ] `go run . 2>&1 | head -30` usage lists the same subcommands as CLAUDE.md Build & Run.
- [ ] No ids, tokens or emails added (grep the diff).
- [ ] go build ./... && go vet ./... && go test ./... green

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
