# M3-acceptance: Independent test

- Milestone: M3
- Branch / worktree: `t/M3-acceptance`, `../YNAB_AutoCategorizer.wt/M3-acceptance`
- Depends on: M3-docs
- Touches (exclusive while running): tests/acceptance/M3/
- Plan refs: §7 M3, R5, R6, R6b, R9b, R11a, N2, N7, N9
- YNAB access: sandbox writes (plan id: `YNAB_SANDBOX_PLAN_ID` in `.env`; **sup copies the main checkout's `.env` into this worktree before starting**), read-only live through hoid's deployed service
- Deploy: no (tests the M3-17 deployment)

## Goal
agy, black-box, confirms M3's safety properties on the running system before anyone considers go-live.

## Scope
- On hoid in dry-run: `write_log` is empty, questions appear, the cap holds, and a click records without changing YNAB.
- Against the sandbox: the writer applies, logs before/after, and refuses a human-edited row. A human edit made in the sandbox stays through a later sync.
- Slack: an unlisted user's click has no effect; a double click applies once; a restart keeps routing.
- `CLASSIFY_MODE=auto` with a mismatched hash behaves as dry-run (local, against the sandbox, never hoid).
- `get_category_decisions` works through the authenticated MCP.
- Secret and id grep over logs, Slack text and DB exports used in the test.

## Out of scope
- Setting `auto` on hoid, or any write to the live plan.

## Acceptance (each must be demonstrably checked)
- [ ] Each Scope item has a script in `tests/acceptance/M3/` and a recorded result.
- [ ] Report at `../_reviews/M3-acceptance.md` with a PASS or FAIL verdict.
- [ ] `ssh hoid 'docker exec finbot-finbot-1 finbot dry-run-report | tail -1'` → `writes: 0` at the end.
- [ ] go build ./... && go vet ./... && go test ./... green

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
