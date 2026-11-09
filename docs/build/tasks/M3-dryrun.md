# M3-dryrun: Dry-run week and evidence

- Milestone: M3
- Branch / worktree: `t/M3-dryrun`, `../YNAB_AutoCategorizer.wt/M3-dryrun`
- Depends on: M3-17
- Touches (exclusive while running): docs/build/reports/
- Plan refs: §7 M3, R6, BACKLOG "What the active plan means for M3"
- YNAB access: none directly (reads hoid's report)
- Deploy: no (smoke: the commands below, run by sup; the service is already deployed)

## Goal
After at least 7 days of dry-run, Brendan has one page of evidence for the go-live decision. It's an observation task: start it when the week is up.

## Scope
- Run on the day the 7th full day ends:
  ```bash
  ssh hoid 'docker exec finbot-finbot-1 finbot dry-run-report'
  ssh hoid 'docker logs --since 168h finbot-finbot-1 2>&1 | grep -c "\"level\":\"ERROR\""'
  ```
- Write `docs/build/reports/M3-dryrun.md`: the report output, model errors, restarts, Slack noise (questions per day), the τ and calibration n in use, the linked-account count, and whether any category changed (candidates hash vs `calibration.json`).
- State the recommendation plainly: go, wait, or rerun calibration. With fewer than 30 judged, say so and give the interval, rather than implying the 85% is confirmed.

## Out of scope
- Changing the mode. Tuning τ by hand.

## Acceptance (each must be demonstrably checked)
- [ ] First and last decision dates in the report span at least 7 days.
- [ ] `writes: 0` is in the report.
- [ ] The report states n, precision and the interval, and names any `auto_eligible` or hash problem.
- [ ] `grep -E '[0-9a-f]{8}-[0-9a-f]{4}' docs/build/reports/M3-dryrun.md` → no matches.

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
