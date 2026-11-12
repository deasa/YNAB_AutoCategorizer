# M3-cats: one-off category restructure command (dry run first)

- Milestone: M3 (pulled forward from M4's seed-plan by Brendan, 2026-11-11)
- Branch / worktree: `t/M3-cats`, `../YNAB_AutoCategorizer.wt/M3-cats`
- Depends on: none
- Touches (exclusive while running): `ynab/` (category write methods only), `cmd/restructure.go` (+ test), `cmd/app.go`
  (dispatch line only), `deploy/restructure/` (the spec file)
- Plan refs: architecture.md §3.3 seed-plan paragraph (line ~109) for the dry-run-then-approve pattern; nothing else
- YNAB access: sandbox writes (`YNAB_SANDBOX_PLAN_ID` in `.env`, sup copies it in). **Live plan: dry run only.** The live
  `--apply` is run by sup, after Brendan approves the exact printed list. The builder never runs it.
- Deploy: no

## Goal
`finbot restructure --spec <file> [--plan <id>] [--apply]` reads a layout spec, compares it with the plan's real category
groups and categories, and prints every change it would make. With `--apply` it makes those changes through the YNAB
API, in order, and prints each result. The spec holds Brendan's approved conscious-spending layout. Source:
the Claude doc "finbot: proposed CSP category layout"; the table below is the authority.

## Scope
- `ynab/`: add `CreateCategoryGroup`, `RenameCategoryGroup`, `CreateCategory`, `UpdateCategory` (name, group, goal target
  and target date only) wrapping the library's `Categories.CreateGroup/RenameGroup/Create/Update`. They go through the
  governor as writes (`asWrite`) with redacted errors, like `PatchTransactions`. Extend the no-id-leak test to cover them.
- Spec file `deploy/restructure/csp-2026-11.json`: by **name**, never by id (the repo is public-safe), listing the groups
  in order with each one's categories in order. Each entry is one of: rename-group, create-group, move, rename+move,
  create, set-target. Encode the layout below.
- `cmd/restructure.go`:
  - Resolve every name against the plan (`GetCategories`). Refuse (exit 1, nothing written) if any spec name is
    missing or ambiguous, or if the plan id equals `YNAB_BUDGET_ID` and `--apply` is set without `--confirm-live`.
  - Print the diff as a numbered list of human-readable lines (group and category names only, never ids). Also list
    every existing, non-deleted category the spec doesn't mention (Credit Card Payments and internal groups excepted),
    so nothing is missed.
  - **Stale targets:** for a category with a target date before today, propose rolling it forward by whole years to the
    first date on or after today. Keep the amount. Each proposed date is its own printed line.
  - With `--apply`: one request per change, in printed order. Stop at the first failure and print what was done and
    what wasn't. Running it again after a partial apply must be safe: already-done changes show as "no change".
  - Before any write, check the rate budget like `tests/acceptance/M3` does (it shares hoid's token). Refuse below 120
    left. Cap the run at 100 requests.
- Tests: unit tests with a fake API for the diff, the refusals, rerun idempotence, the stale-date roll-forward and
  name-only output. Then one real run against the sandbox plan: build a small copy of the old layout in the sandbox,
  dry run, apply, then dry run again showing no changes.

## Layout (Brendan approved 2026-11-11)
Groups, in order. "rename" reuses the existing group, so its id stays.
1. **Giving**: new group. Church - Tithing, Church - Fast Offerings, Giving - Generosity (all moved from Monthly Savings Funds).
2. **Fixed Costs**: rename of Mortgage & Utilities. Home - Mortgage, Home - Highland City Utilities, Home - Electric,
   Home - Natural Gas, Home - Internet (stay); Home - Cell Phone, Home - Insurance, Auto - Insurance (from Monthly
   Savings Funds); Home - Kids Classes & Education (from Monthly Variable Expenses); Subs - Lawn renamed to
   Home - Lawn Service; Subs - Spotify, Subs - Disney+, Subs - Amazon Prime, Subs - YNAB, Subs - Canva,
   Subs - Google Domains Website, Subs - TrainerRoad (from Subscriptions); Google One renamed to Subs - Google One;
   new: Groceries, Gas & Transport, Household & Kids.
3. **Investments**: rename of Financial Independence. Stock Investments (stays); new: Retirement (tracking only, no target:
   contributions come out pre-tax).
4. **Savings Goals**: rename of Monthly Savings Funds. Wise - Buffer, Wise - Emergency Preparedness, Wise - Medical &
   Dental (from Target Balance Goals); Auto - Replacement, Auto - Repairs and Expenses, Home - Tech / Computer Replacement,
   Home - Improvement / Maintenance, Holiday - Christmas, Holiday - Halloween, Birthday - Benson/Elliott/Marie Gifts,
   Birthday - Benson/Elliott/Marie Party (stay).
5. **Guilt-Free**: rename of Fun Money. Fun - Brendan Under The Radar, Fun - Hilarie Under The Radar, Fun - Larks Dues,
   Fun - Next Trip, Voice Lessons (stay); new: Dining Out, Shopping.
6. **Retired**: rename of Budget Meeting. Discretionary, Talk About, Needs a job! (stay); Home - Riverton City Utilities
   (from Monthly Variable Expenses). Brendan hides this group and the emptied Subscriptions, Monthly Variable Expenses and
   Target Balance Goals in YNAB afterwards (the API can't hide).

## Out of scope
- Moving or recategorizing any transaction. `PatchTransactions` is not called.
- Hiding or deleting anything, and assigning money.
- The classifier policy (`classify/policy.json`): that's a separate task.
- `finbot seed-plan` (M4). Keep this command self-contained so M4 can reuse or delete it.
- Running `--apply` against the live plan.

## Acceptance (each must be demonstrably checked)
- [ ] Unit tests: diff, missing/ambiguous-name refusal, live-plan refusal without `--confirm-live`, rerun → all
      "no change", stale-date roll-forward, output contains no ids (UUID grep) → pass
- [ ] Sandbox: dry run → apply → dry run again shows 0 changes; transcript (names only) in the Result
- [ ] Live **dry run only** (`--plan` = `YNAB_BUDGET_ID`, no `--apply`): full printed list saved to
      `../_reviews/M3-cats-live-dryrun.txt`. It must show 0 unresolved names and list any category the spec doesn't mention.
- [ ] `go build ./... && go vet ./... && go test ./...` green

## After merge (sup)
Brendan reads the live dry-run list and approves it. Then sup runs, from the main checkout:
`./finbot restructure --spec deploy/restructure/csp-2026-11.json --plan "$YNAB_BUDGET_ID" --apply --confirm-live`
then the dry run again (expect 0 changes), and asks Brendan to hide the emptied groups.

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->

## Result
- Status: DONE
- What changed:
  - `ynab/client.go`: `CreateCategoryGroup`, `RenameCategoryGroup`, `CreateCategory` (name + group, no target),
    `UpdateCategory` (`CategoryEdit`: name, group, goal target, target date; only set fields sent). All preflight as
    writes and go through `asWrite` with redacted errors. Re-exports `Milliunits`, `Date`, `NewDate`, `ParseDate`.
    Tests: request shapes (`TestClient_CategoryWrites`) and all four added to `TestClient_NoErrorPathLeaksIDs`.
  - `cmd/restructure.go` (+ `restructure_test.go`): `finbot restructure --spec <file> [--plan <id>] [--apply
    [--confirm-live] --used <n> [--ledger <path>]]`. Resolves by name (internal groups and Credit Card Payments skipped),
    prints a numbered change list plus "no change" lines in spec order, the "Not in the spec" list, `unresolved: N` and
    `changes: N`. Refusals (exit 1, nothing written): any missing/ambiguous/conflicting name; `--apply` on
    `YNAB_BUDGET_ID` without `--confirm-live` (checked before any request); estimated < 120 requests left; more than 100
    requests in the run. A frozen-clock governor of 100 caps HTTP attempts (retries included) as a backstop.
  - Spec ops: groups `create-group` / `rename-group` (`from`) / `keep-group`; categories `stay`, `move` (`from_group`),
    `rename+move` (`from`, `from_group`; `from_group` = the group's old name makes it a plain rename), `create`,
    `set-target` (`target: {milliunits, date}`; `target` may go on any op). Unknown fields are rejected.
  - Stale targets: every spec category whose target date is before today gets its own line rolling it forward by whole
    years (29 Feb → 28 Feb in common years), sending the unchanged `goal_target` alongside the new date.
  - `deploy/restructure/csp-2026-11.json`: the approved layout, names only. `cmd/app.go`: dispatch line + usage.
- Acceptance:
  - Unit tests → pass: `go test ./cmd -run 'TestRestructure_|TestRollForward|TestMoney'` (diff against the live
    layout's names; missing category/group, ambiguous category/group and create-collision refusals with 0 writes;
    live-plan refusal with 0 requests; apply then rerun → `changes: 0`; partial failure at write 5 then resume then
    0 changes; rate-budget refusal at 109 left; stale roll-forward incl. leap days; every test's output UUID-grepped).
  - Sandbox → pass. Built a small old layout with the command itself (16 writes: 4 groups, 9 categories, 2 stale
    targets dated 2026-06-28 and 2020-12-01, which YNAB accepted). Then, names only:
    dry run `changes: 13` → `--apply` `done: 13 of 13` → dry run again `unresolved: 0`, `changes: 0`. A GET afterwards
    showed the rolled targets kept their amounts and goal type (Home - Insurance $800.00 by 2027-06-28 NEED; Holiday -
    Christmas $1,500.00 by 2026-12-01 NEED). Transcript excerpt (apply):
    ```
      1. done: create group "Giving"
      2. done: move "Church - Tithing" from "Monthly Savings Funds" to "Giving"
      3. done: rename group "Mortgage & Utilities" to "Fixed Costs"
      4. done: move "Home - Insurance" from "Monthly Savings Funds" to "Fixed Costs"
      5. done: roll stale target date of "Home - Insurance" forward from 2026-06-28 to 2027-06-28 (target $800.00 kept)
      6. done: rename "Subs - Lawn" to "Home - Lawn Service" and move it from "Subscriptions" to "Fixed Costs"
      7. done: move "Subs - Spotify" from "Subscriptions" to "Fixed Costs"
      8. done: rename "Google One" to "Subs - Google One" and move it from "Subscriptions" to "Fixed Costs"
      9. done: create "Groceries" in "Fixed Costs"
     10. done: rename group "Monthly Savings Funds" to "Savings Goals"
     11. done: roll stale target date of "Holiday - Christmas" forward from 2020-12-01 to 2026-12-01 (target $1,500.00 kept)
     12. done: rename group "Fun Money" to "Guilt-Free"
     13. done: create "Dining Out" in "Guilt-Free"
    done: 13 of 13
    ```
    The rerun printed 15 "no change" lines, "Subs - Bark" as not in the spec, `changes: 0`, exit 0.
  - Live dry run → pass: `../_reviews/M3-cats-live-dryrun.txt` (no `--apply`; 1 GET). `unresolved: 0`, `changes: 45`
    (1 group create, 5 group renames, 21 moves/renames, 5 creates, 13 stale-date rolls), 16 categories not in the spec
    listed. UUID and token grep of the file: 0 hits.
  - `go build ./... && go vet ./... && go test ./...` → green.
- Deviations from brief:
  - "Retirement" (spec: new) already exists in Financial Independence, so it shows as no change rather than a create.
  - Categories can't be ordered within a group through the API; the spec's order is printed but not applied (the
    output says so).
  - Rate budget: YNAB sends no header, so like the M3 suite the estimate is 200 − `--used` (hoid's last-hour requests,
    doubled, supplied by the caller) − the optional M3 ledger's last hour − 10 margin − this run's requests. `--used`
    is required with `--apply`. My brief lists no hoid steps, so for the sandbox runs I used `--used 10` (4 syncs/h +
    an import, doubled) with the M3 ledger (last entry ~2h old); the runs were appended to that ledger.
  - The stale-target check covers the categories the spec names, not the unmentioned ones.
- Follow-ups for backlog:
  - Sandbox plan now holds the test layout (Giving, Fixed Costs, Savings Goals, Guilt-Free, Subscriptions). Harmless;
    delete in YNAB if wanted.
  - The live list puts unmentioned categories in groups that get renamed (e.g. "Buffer"/"Reimbursement" end up in
    Retired, "Business Bike " and two hidden trips in Guilt-Free, "Mortgage Payoff" in Investments, two hidden ones in
    Savings Goals). Brendan should confirm that's fine when he reads the list.
  - "Fun - Larks Dues" rolls to 2026-11-18, one week out.
- Docs follow-ups:
  - CLAUDE.md Build & Run: add `./finbot restructure --spec <file> [--plan <id>] [--apply [--confirm-live] --used <n>
    [--ledger path]]`.
  - CLAUDE.md `ynab/` role and Safety: the YNAB writes are no longer only `ImportTransactions` and `PatchTransactions`;
    add the four category writes, called only by `finbot restructure` (a manual one-off, never by serve).
- Live apply command for sup (after Brendan approves; ~46 requests):
  ```
  hn=$(ssh hoid 'docker logs --since 60m finbot-finbot-1 2>&1' | grep -cE '"msg":"(sync tick|import tick|import failed|import done)"')
  ./finbot restructure --spec deploy/restructure/csp-2026-11.json --plan "$YNAB_BUDGET_ID" --apply --confirm-live \
    --used $((hn * 2)) --ledger ../_m3-acceptance-state/ynab-ledger
  ```
