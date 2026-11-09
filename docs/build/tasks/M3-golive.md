# M3-golive: Auto at τ

- Milestone: M3
- Branch / worktree: `t/M3-golive`, `../YNAB_AutoCategorizer.wt/M3-golive`
- Depends on: M3-dryrun, M3-acceptance, and **Brendan's explicit go, recorded in STATE.md Decisions**
- Touches (exclusive while running): deploy/
- Plan refs: §3.2 step 4, §7 M3, R6, N7
- YNAB access: live writes through `writer/` once deployed, on Brendan's go only
- Deploy: yes (sup runs it only after that go)

## Goal
The hoid service categorizes above τ on its own, with every write logged. This is the first autonomous categorization of the live plan.

## Scope
- Do not start without the STATE.md decision. Don't infer the go from a passing report.
- `deploy/compose.yaml`: `CLASSIFY_MODE: "auto"`. Add `CLASSIFY_AUTO_SINCE` set to the go-live date unless Brendan chose to include the backlog. Dry-run answers aren't written to YNAB retroactively.
- `calibration.json` must have `auto_eligible=true` and match the current candidates hash. If it doesn't, stop and rerun M3-6 rather than editing the file.

## Out of scope
- Any other compose change. Changing τ. M4 work.

## Acceptance (each must be demonstrably checked)
- [ ] The STATE.md decision exists and is quoted in Result.
- [ ] `git diff main --stat` shows only `deploy/compose.yaml` and this brief.
- [ ] `calibrate/calibration.json` has `auto_eligible` true.
- [ ] go build ./... && go vet ./... && go test ./... green

## Deploy steps / smoke check (only if Deploy: yes)
```bash
./deploy/deploy.sh
ssh hoid 'docker inspect -f "{{.State.Health.Status}}" finbot-finbot-1'    # healthy
ssh hoid 'docker exec finbot-finbot-1 printenv CLASSIFY_MODE'              # auto
ssh hoid 'docker logs --since 20m finbot-finbot-1 2>&1 | grep "effective_mode" | tail -1'   # effective mode auto, no downgrade warning
ssh hoid 'docker exec finbot-finbot-1 finbot dry-run-report | tail -1'     # writes: <n>, and n grows only with applied decisions
```
Backout: set `CLASSIFY_MODE` back to `dry_run` in compose and redeploy. Past writes stay, and each one has before_json in `write_log`.
Watch the first writes for one day, and check one applied row in YNAB with Brendan.

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->
