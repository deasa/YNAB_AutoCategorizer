# M3-simplify: Fewer, broader categories (proposal + replay)

- Milestone: M3 (before go-live)
- Branch / worktree: `t/M3-simplify`, `../YNAB_AutoCategorizer.wt/M3-simplify`
- Depends on: M3-dryrun (merged; read `docs/build/reports/M3-dryrun.md` first)
- Touches (exclusive while running): docs/build/reports/, deploy/restructure/ (new spec file only)
- Plan refs: R6, R8, R8a, R10; STATE.md → Decisions 2026-11-22
- YNAB access: none. Read-only copy of hoid's DB (the safe copy in `tests/acceptance/M3/hoid_check.sh`). No
  `restructure --apply`, no `finbot calibrate`, no writes anywhere.
- Deploy: no

## Goal
Brendan gets one page that proposes a smaller category list in Ramit's Conscious Spending Plan shape, shows what it
would have done to the dry-run precision, and comes with a `finbot restructure` spec he can approve as-is.

## Brendan's decisions (2026-11-22)
- Guilt-Free is **one shared category** for discretionary spending (dining out, shopping, entertainment and the like).
- Fun money is fully free spending. finbot never picks `Fun - Brendan Under The Radar` or `Fun - Hilarie Under The
  Radar` for a transaction. At the monthly budget meeting (R10, M4), each of them says what their fun money went to and
  the budget is updated then. Those two categories stay as budget buckets.
- Fixed Costs stay granular (recurring payees are what the payee rules handle), but near-neighbour categories that
  the model confuses get merged.

## Scope
1. From the DB copy: every visible category in the active plan, by group, with the number of labelled transactions
   (human labels, `store/labeled_read.go` semantics) and the dry-run confusions (`decisions` vs the next human label,
   as `calibrate.BuildDryRunReport` judges them).
2. Propose the new list. For each current category: keep, merge into X, move to group Y, or hide. Always handle:
   Groceries vs Household & Kids (give both options with numbers: merged, or kept apart with a one-line rule),
   Voice Lessons vs Kids Classes & Education, subscriptions, Dining Out/Shopping into Guilt-Free, and what's left in
   the Guilt-Free group (Larks Dues, Next Trip, Business Bike, Voice Lessons...). Savings goals stay set-asides.
3. Replay: re-score the current-hash dry-run decisions with predictions and labels both mapped through the merge
   map. Report old vs new precision with Wilson intervals and say plainly how small n is.
4. Calibration impact. Merged and hidden categories' history labels currently become hard negatives (labels outside
   the candidates). Say how many labels that affects, and whether calibration needs a label alias map (old category →
   merged category) to use them. Describe the change; don't build it.
5. How the two fun categories leave the classifier: as set-asides in `classify/policy.json`, or by exclusion in
   `Candidates`. Describe; don't build. Also include YNAB's `Needs a job!` placeholder exclusion (M3-dryrun follow-up).
6. Write `deploy/restructure/csp-2026-11b.json` in the existing spec format (see `csp-2026-11.json` and `cmd`'s
   restructure parser). Merges are "hide the old category", and the spec can't move past transactions: say so.
7. Write `docs/build/reports/M3-simplify.md`: the proposed list (one table per CSP group), the replay numbers, the
   calibration impact, the policy changes needed, and the order of operations for Brendan.

## Out of scope
- Any write to YNAB, the sandbox or hoid. Code changes to `classify/`, `calibrate/` or `slack/` (follow-up tasks).
- The monthly meeting itself (M4, R10).

## Acceptance (each must be demonstrably checked)
- [ ] `./finbot restructure --spec deploy/restructure/csp-2026-11b.json --plan <active plan>` (dry run, no `--apply`)
      parses and prints its change list; the count is in the report. Check `--used` needs first; this is a GET-only run.
- [ ] Replay table shows old and new precision with Wilson intervals and n.
- [ ] Neither fun category appears as a classifier target in the proposal.
- [ ] `grep -E '[0-9a-f]{8}-[0-9a-f]{4}' docs/build/reports/M3-simplify.md deploy/restructure/csp-2026-11b.json` → no matches.
- [ ] go build ./... && go vet ./... && go test ./... green

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
