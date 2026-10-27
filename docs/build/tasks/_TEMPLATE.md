# <ID>: <title>

- Milestone: M<n>
- Branch / worktree: `t/<ID>`, `../YNAB_AutoCategorizer.wt/<ID>`
- Depends on: <task IDs merged to main | none>
- Touches (exclusive while running): <packages/dirs>
- Plan refs: <R*, N*, §x.y — read only these>
- YNAB access: none | read-only live | sandbox writes (plan id: <id>)
- Deploy: no | yes (steps below)

## Goal
<2-4 sentences: what exists after this task that didn't before>

## Scope
- <concrete item>
- <concrete item>

## Out of scope
- <things a builder might be tempted to do>

## Acceptance (each must be demonstrably checked)
- [ ] <test / command → expected result>
- [ ] go build ./... && go vet ./... && go test ./... green

## Deploy steps / smoke check (only if Deploy: yes)
<exact commands for sup>

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
