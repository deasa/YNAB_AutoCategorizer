# M2-inv1: why the MCP sees only one credit card

- Milestone: M2 (investigation, no code change)
- Branch / worktree: `t/M2-inv1`, `../YNAB_AutoCategorizer.wt/M2-inv1`
- Depends on: none
- Touches (exclusive while running): nothing in git except this brief's Result
- Plan refs: none needed
- YNAB access: read-only live (GETs only, a handful; the governor's 200/h budget is shared with hoid's sync)
- Deploy: no

## Goal
Brendan asked claude.ai (through finbot's MCP) about October groceries. It found no Groceries category and no grocery
transactions, said only three accounts are bank-linked (all fine), and can see only **one credit card**. Find out why,
and say whether it's a finbot bug or a data/plan fact.

## Scope
- Build a local mirror: `go build -o finbot . && FINBOT_DB_PATH=<scratch>/inv.db SLACK_BOT_TOKEN= HEALTHCHECK_PING_URL=
  BACKUP_AGE_RECIPIENT= BACKUP_TURSO_URL= BACKUP_TURSO_TOKEN= IMPORT_ENABLED=false ./finbot sync`. Never run `serve` with
  the real Slack/backup values.
- From the YNAB API for the synced plan (`YNAB_BUDGET_ID`), list every account: name, type, on_budget, closed, deleted,
  direct_import_linked, direct_import_in_error, and the latest transaction date. Compare with the local mirror's accounts
  table. Do they match (count and flags)?
- Run the MCP read tools' store queries (or the tools in-process from a Go test file you don't commit) that answer
  "which credit cards exist" and "data freshness": which accounts do they include or filter out (closed, off-budget,
  unlinked, untagged)? Is a credit card dropped that shouldn't be?
- From `sync --archived` / `DB.PlanScans` or `GET /plans`: which other plans have open credit-card accounts with
  transactions after 2026-03-14? Names and latest dates only. That says whether Brendan's current cards live elsewhere.
- Also report, for October 2026: transaction count per account in the mirror.

## Out of scope
- Fixing anything. If it's a finbot bug, describe it precisely (file:line, failing case) so sup can brief a fix task.
- Any YNAB write. Printing ids, the token or the plan id (account and plan names are fine).

## Acceptance (each must be demonstrably checked)
- [ ] Table: YNAB accounts vs mirror accounts, with the flags above, and the October transaction counts
- [ ] Which MCP tool/query filters which accounts, with file:line
- [ ] Other plans with recently active credit cards (names, latest dates)
- [ ] Conclusion in one paragraph: finbot bug (where) or data fact (what Brendan would change in YNAB)
- [ ] Output grepped for UUIDs and the token before it goes into the Result
