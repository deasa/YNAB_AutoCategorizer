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

## Result
- Status: DONE
- Plan investigated: **Trying auto-categorizer**, per sup's update after Brendan changed `YNAB_BUDGET_ID` mid-task.

### Conclusion
Not a finbot bug: this is a data fact. For Trying auto-categorizer the mirror matches YNAB exactly, and
`get_financial_snapshot` returns all 10 open credit cards (10 in YNAB, 10 from the tool, same names). Only one card, B
Amazon Prime Visa, has any October transaction (3, none imported), so a question about October spending can only surface
that card. The other 9 open cards last have transactions between 2026-03-14 and 2026-06-15. No credit card here is
bank-linked, so `data_freshness` correctly lists just the 3 linked accounts. The plan has no Groceries (or food-named)
category, and 27 of its 38 October transactions have no category. Brendan's active spending is elsewhere: **Make Money
Work** has Cash Back Visa – 7594, bank-linked, 35 October transactions, latest 2026-11-07, and transactions in a grocery
category. To get that into claude.ai, Brendan would point `YNAB_BUDGET_ID` at Make Money Work and redeploy. That is
Brendan's call, and it also moves hoid's scheduled import POST (`IMPORT_ENABLED=true`) to that plan. Keeping Trying
auto-categorizer instead means linking the current cards and adding the categories in YNAB.

### YNAB vs mirror, Trying auto-categorizer
Fresh DB, then a separate YNAB export: 24 accounts each, every field below `diff`-identical. Mirror has 310 live
transactions, 2026-03-14 to 2026-11-07, 38 in October.

| Account | Type | On budget | Closed | Deleted | Linked | In error | Latest txn | Oct txns |
|---|---|---|---|---|---|---|---|---|
| Ally Interest Checking | checking | yes | no | no | yes | no | 2026-10-29 | 6 |
| Ally Savings Account | savings | yes | no | no | yes | no | 2026-11-06 | 10 |
| AmFirst Checking | checking | yes | yes | no | no | no | - | 0 |
| AmFirst Emergency | checking | yes | no | no | no | no | 2026-03-14 | 0 |
| AmFirst Savings | savings | yes | no | no | no | no | 2026-03-14 | 0 |
| B Amazon Prime Visa | creditCard | yes | no | no | no | no | 2026-11-06 | 3 |
| B Freedom Unlimited | creditCard | yes | no | no | no | no | 2026-03-26 | 0 |
| B IHG Rewards Premier | creditCard | yes | no | no | no | no | 2026-03-14 | 0 |
| B Ink Business | creditCard | yes | yes | no | no | no | - | 0 |
| B SW Prem Biz | creditCard | yes | no | no | no | no | 2026-03-14 | 0 |
| B Sapphire Preferred | creditCard | yes | yes | no | no | no | - | 0 |
| B Sapphire Preferred 2024 | creditCard | yes | no | no | no | no | 2026-03-25 | 0 |
| B World of Hyatt | creditCard | yes | yes | no | no | no | - | 0 |
| BH Ashton Venmo | checking | yes | no | no | yes | no | 2026-11-07 | 19 |
| H - Rapid Rewards Plus | creditCard | yes | no | no | no | no | 2026-03-14 | 0 |
| H Hyatt Visa | creditCard | yes | no | no | no | no | 2026-03-14 | 0 |
| H IHG Premier | creditCard | yes | no | no | no | no | 2026-03-14 | 0 |
| H SW Perf Biz | creditCard | yes | no | no | no | no | 2026-03-14 | 0 |
| Health Savings Account | savings | yes | no | no | no | no | 2026-03-14 | 0 |
| Home - Mortgage | mortgage | no | no | no | no | no | 2026-03-14 | 0 |
| Investments | otherAsset | no | yes | no | no | no | - | 0 |
| PayPal | creditCard | yes | yes | no | no | no | - | 0 |
| PayPals | checking | yes | no | no | no | no | 2026-03-14 | 0 |
| Venture | creditCard | yes | no | no | no | no | 2026-06-15 | 0 |

### Which MCP tool or query filters which accounts
Results are from the real tool functions, run in-process on the fresh DB. No open credit card is dropped.

| Tool / query | Filter | Where | Result on this plan |
|---|---|---|---|
| `data_freshness` account list (on every response) | linked and not closed | `mcp/freshness.go:79`; deleted out at `sync/mirror.go:187` | Ally Interest Checking, Ally Savings Account, BH Ashton Venmo, all ok; `links_ok` true |
| `get_financial_snapshot` accounts | not deleted, not closed; untagged stays in, as bucket `untagged` | `store/snapshot_read.go:35`, `mcp/tools_snapshot.go:331` | 18 of 24 accounts; all 10 open cards |
| `query_transactions` | deleted lines only, no account filter unless the caller passes one | `store/transactions_read.go:24`, `:33` | October: Ally Interest Checking 6, Ally Savings Account 10, B Amazon Prime Visa 3, BH Ashton Venmo 19 (38) |
| `compare_spending` (`CategoryTotals`) | on-budget, no transfers or Inflow | `store/transactions_read.go:198` | Not run; no open card is off-budget |
| Snapshot cash flow (`MonthlyFlows`) | on-budget, no transfers | `store/snapshot_read.go:88` | Not run; no open card is off-budget |

`query_transactions` with category `Groceries` returns `no category is named "Groceries"`, which is correct for this plan.

### Other plans with open credit cards active after 2026-03-14

| Plan | Card | Linked | Latest txn | Oct txns |
|---|---|---|---|---|
| Make Money Work | Cash Back Visa – 7594 | yes | 2026-11-07 | 35 |
| Jayla's Plan | Credit Card – 8810 | yes | 2026-11-06 | 60 |
| Penny snatcher (Archived on 2026-10-15) | Cash Back Visa – 7594 | no | 2026-10-14 | 45 |
| May 2025 (Archived on 2026-03-14) | B Amazon Prime Visa | no | 2026-11-06 | 3 |
| May 2025 (Archived on 2026-03-14) | Venture | no | 2026-06-15 | 0 |
| May 2025 (Archived on 2026-03-14) | B Freedom Unlimited | no | 2026-06-14 | 0 |
| May 2025 (Archived on 2026-03-14) | B Sapphire Preferred 2024 | no | 2026-06-11 | 0 |

Coverage: I fetched transactions for the 4 plans modified since 2026-01-01 or holding a linked open card. The other 10
plans were not fetched: all were last modified by 2025-05-18 and none has a linked open card. `last_modified_on` can lag
(May 2025 reads 2026-07-13 but has 2026-11 transactions), so that exclusion is strong evidence, not proof.

### Note: previous plan (May 2025, Archived on 2026-03-14)
Its mirror also matched YNAB (24 accounts, 2,074 transactions, identical fields). Same outcome: 10 open cards in the
snapshot, the same 3 linked accounts, the same October counts, no Groceries category.

### Checklist
- What changed: this Result only. No code change. The throwaway `mcp/zz_inv_test.go` was deleted and never committed.
- Acceptance:
  - Accounts table with flags and October counts: pass (`diff` of YNAB export vs mirror, identical)
  - Tool filters with file:line: pass (real tool functions run in-process, plus code read)
  - Other plans: pass, with the coverage limit above (`GET /plans?include_accounts=true`, plus 4 date-limited GETs)
  - Conclusion: pass (above)
  - UUID and token grep: pass (run on this file before commit)
  - `go build ./...`, `go vet ./...`, `go test ./...`: pass
- Deviations from brief:
  - The plan changed mid-task, so the main answer covers the new plan and the old one is a note (sup's instruction).
  - 12 YNAB GETs in total, more than "a handful": 3 syncs, 4 plan-level calls, 1 plan list, 4 date-limited. Two of the
    plan-level calls were accidental: an inline comment in `.env` broke my URL and each returned the full export.
  - Did not check hoid directly. Hoid's 24 accounts and 310 transactions come from sup, and they match this mirror.
- Follow-ups for backlog: decide which plan finbot should follow (see Conclusion). Optional: have `data_freshness` say
  that it lists linked accounts only, since "only three accounts" reads as if the others are missing.
- Docs follow-ups: none
