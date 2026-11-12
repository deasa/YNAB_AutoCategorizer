# M3-policy: classifier policy for the new category layout

- Milestone: M3 (follows M3-cats, Brendan 2026-11-12)
- Branch / worktree: `t/M3-policy`, `../YNAB_AutoCategorizer.wt/M3-policy`
- Depends on: M3-cats merged (the spec `deploy/restructure/csp-2026-11.json` is on main)
- Touches (exclusive while running): `classify/policy.json`, `classify/policy.go` (only if needed), `classify/*_test.go`
- Plan refs: none; read `classify/policy.go`, `classify/candidates.go` (set-aside matching and the "matches no
  category" WARN)
- YNAB access: none (a read-only live check is allowed, see Acceptance)
- Deploy: yes, but only **after** sup has applied the restructure to the live plan (sup sequences it)

## Goal
The embedded policy matches Brendan's new conscious-spending layout. Everyday spending now has real categories, so the
Discretionary description goes, and each new category gets a short description that steers the model. On the new
layout, the set-aside keywords match real categories and log no WARN.

## Decisions (Brendan, 2026-11-12)
- Set-asides stay out of the classifier: finbot never suggests a savings-goal fund (birthdays, holidays, trips) on its
  own. A person can still pick one in Slack.
- Retired categories (Discretionary, Talk About, Needs a job!, in the hidden "Retired" group) are never targets. Hiding
  already excludes them; the policy shouldn't mention them.

## Scope
- `descriptions`: drop Discretionary. Add one or two plain sentences each for Groceries, Gas & Transport,
  Household & Kids, Dining Out and Shopping. Make the boundaries explicit: groceries vs dining out (takeout and coffee
  are Dining Out), Amazon and general wants go to Shopping, toiletries, clothing and kids' supplies go to Household & Kids,
  and fuel, parking, tolls and rideshare go to Gas & Transport. Add Reimbursement (work or other expenses to be paid
  back). Mention that a gift bought for an event still goes to Shopping unless a person picks the event fund.
- `set_aside_keywords`: they must match the new layout's event funds: Birthday (Gifts and Party), Holiday (Christmas,
  Halloween) and Fun - Next Trip. Drop keywords that match nothing ("Vacation"). Don't let "Giving" exclude Church -
  Tithing or Fast Offerings (word matching already doesn't; keep it that way). Also make the never-spent-from tracking
  and investment lines non-targets: Retirement, Stock Investments and Mortgage Payoff. Use keywords or another explicit
  field, whichever is cleaner, and document the choice in `policy.go`'s comment.
- Bump `version`. The policy hash changes, so the existing calibration stops applying (τ is null anyway, BA-20).

## Out of scope
- The restructure itself, and calibrate's label-plan logic (re-calibration is BA-20's separate step).
- Slack, writer, pipeline code.

## Acceptance (each must be demonstrably checked)
- [ ] Unit: build a candidates fixture from the spec's layout (names and groups, no ids) and assert: Groceries, Dining
      Out, Shopping, Household & Kids, Gas & Transport, Reimbursement, Home - Mortgage, Subs - Spotify, Church - Tithing are
      candidates; every Birthday, Holiday and Fun - Next Trip category, Retirement, Stock Investments and Mortgage Payoff
      are not; no "matches no category" WARN → pass
- [ ] `go test ./classify/... ./calibrate/...` green; the policy hash changed
- [ ] `go build ./... && go vet ./... && go test ./...` green

## Deploy steps / smoke check (sup, after the live restructure)
`./deploy/deploy.sh`. Then: healthy, dry_run, **0 `policy warning` WARN lines** on the next categorize run, 0 ERROR,
`writes: 0`. New questions offer the new categories (no Discretionary).

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
