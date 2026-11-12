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
