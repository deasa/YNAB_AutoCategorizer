# M3-jevctx: CSP research and a review of what Jev sees

- Milestone: M3 (before go-live)
- Branch / worktree: `t/M3-jevctx`, `../YNAB_AutoCategorizer.wt/M3-jevctx`
- Depends on: M3-simplify (merged; read `docs/build/reports/M3-simplify.md` first)
- Touches (exclusive while running): docs/build/reports/
- Plan refs: R6, R8, R8a, R10; STATE.md → Decisions 2026-11-22
- YNAB access: none. Read-only copy of hoid's DB if needed (the copy loop in `tests/acceptance/M3/hoid_check.sh`).
  No Jev calls beyond `JEV_LIVE=1` sized probes (at most 3 small batches), no writes anywhere.
- Deploy: no

## Goal
Brendan gets one report that (1) summarizes Ramit Sethi's Conscious Spending Plan and the budget categories it uses,
(2) audits everything finbot sends Jev for a categorization and lists the holes, and (3) proposes a final,
Jev-optimized category list and context, plus a plan for a labelling session where Brendan answers 200+ historical
transactions against that list to calibrate.

## Scope
1. **CSP research (web).** Ramit's four buckets and their target percentages, the sub-categories he names or shows in
   his own examples and spreadsheet, how he treats guilt-free money and per-person fun money, savings goals vs
   investments, and the monthly review. Cite sources (ramit.com / iwillteachyoutoberich.com pages, the book). Mark
   what is Ramit's and what is a common adaptation.
2. **What Jev sees today.** Load the `typesafe:typesafe-ai` skill first. Then trace one categorization end to end in
   `classify/` and `classify/jev/`: the question text, the state/fields sent (payee, memo, amount, account, date,
   history), the candidate list (`Candidates`), category names, group names, `policy.json` descriptions and set-aside
   keywords, and how the answer and confidence come back. Quote the real request shape with fake values.
3. **Holes.** Anything Jev can't know but needs: ambiguous or overlapping names, categories with no description,
   descriptions that don't separate near neighbours, group context missing, account context (whose card, which
   bucket), amount or recurrence signals, history that's sparse or uses old names, set-asides and placeholders that
   leak in, the fun categories. Use the M3-dryrun and M3-simplify confusions as evidence. Rank by expected impact.
4. **Proposal.** Starting from M3-simplify option A and Brendan's decisions: the final category list per CSP bucket,
   a name and a one-line Jev description for each, what to exclude from candidates, and any change to the request
   (more fields, better question wording) per the skill's guidance. Say which changes are policy/data only and which
   need code. Note where it differs from `deploy/restructure/csp-2026-11b.json`.
5. **Labelling session plan.** Brendan will sit down and label 200+ historical transactions against the new list.
   Propose how: which transactions (sampling so the tuning set has 200+ model-band rows and covers every category),
   the interface (CLI prompt, Slack, or a local page), how answers are stored as labels calibration uses, roughly how
   long it takes, and what code that needs. Describe; don't build.
6. Write `docs/build/reports/M3-jevctx.md`: sections 1–5, short. Lead with the top holes and the decisions Brendan
   needs to make.

## Out of scope
- Code changes, YNAB writes, policy.json edits, running calibrate, restructure `--apply`.

## Acceptance (each must be demonstrably checked)
- [ ] CSP section cites at least 2 sources by URL.
- [ ] The request shape section matches the code (file:line refs) with fake values only.
- [ ] Every hole cites evidence (code, report numbers or a DB count).
- [ ] `grep -E '[0-9a-f]{8}-[0-9a-f]{4}' docs/build/reports/M3-jevctx.md` → no matches; no payee names from real
      transactions beyond generic merchants (no people's names).
- [ ] go build ./... && go vet ./... && go test ./... green

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
