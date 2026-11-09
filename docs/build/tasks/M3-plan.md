# M3-plan: expand M3 into dispatchable task briefs

- Milestone: M3 (gate breakdown; Brendan said "continue" 2026-11-08)
- Branch / worktree: `t/M3-plan`, `../YNAB_AutoCategorizer.wt/M3-plan`
- Depends on: M2 complete (main)
- Touches (exclusive while running): `docs/build/BACKLOG.md` (M3 section only), new `docs/build/tasks/M3-*.md`
- Plan refs: requirements.md (R6*, R7*, R8*, any Slack/classifier items); architecture.md §2, §3.2-§3.4, §5, §5a, §6, §7
  (milestones); spikes jev.md, slack.md, claude-cli.md (+ claude-cli-review*.md MUSTs); STATE.md → Decisions
- YNAB access: none (reading docs and code only)
- Deploy: no

## Goal
Replace the M3 placeholder in BACKLOG.md with a dependency-ordered task table, and write one brief per task from
`tasks/_TEMPLATE.md`, each sized for ~25% of a builder's context (1-2 packages, clear acceptance tests, no "and also").
sup dispatches from these after agy attacks the breakdown.

## Scope
- BACKLOG M3 table in the M2 table's format (ID, Title, Depends, Touches, Notes), mark it `[expanded 2026-11-08]`.
  No two tasks that could run in parallel may touch the same package.
- Briefs `docs/build/tasks/M3-<n>.md` (split `a`/`b` where needed), each with: YNAB access level, Deploy yes/no + exact
  smoke check, acceptance tests, out-of-scope.
- Cover what the plan puts in M3, at least: calibration (old→new category mapping), rules + Jev classifier with τ, the
  decision log, the writer + `write_log` (only YNAB write path besides import), human locks / never override human edits,
  Slack questions (Socket Mode, `questions` table per slack.md §2), alerts and freshness to Slack, the headless `claude`
  fallback (§5a flags exactly as the spike MUSTs say), dry-run mode for one week, then auto at τ behind a flag that only
  Brendan turns on. Plus `get_category_decisions` (deferred from M2) and an M3-docs and M3-acceptance row.
- Fold in the carried follow-ups that touch the same code (list each and say which task takes it, or "deferred"):
  import outcomes only reach logs (surface in Slack/status), import timer resets per deploy, `deploy/rollback.sh` has no
  spike left (retire or rewrite), ScanDiscretionary omits a failed plan from its list, wrong-PKCE auth code counts in
  ActiveTokens for 5 min, unbounded shutdown wait (matters once the LLM job exists), drop txn.ID from op strings,
  slack-go v0.32.0 needs Go 1.26 (already on 1.26: verify).
- Active plan is now **"Trying auto-categorizer"** (STATE.md Decisions, 2026-11-08): only 3 linked accounts, ~38 txns in
  Oct 2026, many uncategorized, no Groceries category. Say what that means for calibration and the dry-run week.
- A "Brendan inputs" list: every decision or account action M3 needs from him (e.g. the category list/mapping is
  PROPOSED until he approves it; BA-9 sandbox plan for write tests; Slack app scopes; going live after dry-run), and the
  first task that blocks on each.

## Out of scope
- Writing any code. Changing requirements.md or architecture.md (list disagreements under "Plan questions" instead).
- Briefs for M4+.

## Acceptance (each must be demonstrably checked)
- [ ] BACKLOG.md M3 table present; every task has a brief file; dependencies form a DAG (list a valid order)
- [ ] Each brief names its packages and at most 2 of them; parallel-runnable tasks share none (show the check)
- [ ] Carried follow-ups table: each item → task or deferred
- [ ] Brendan inputs list with the blocking task for each; Plan questions list (may be empty)
- [ ] No ids, tokens or emails in anything written (grep)
