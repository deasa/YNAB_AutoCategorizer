# Personal Finance Assistant — Requirements (v5 — FINAL candidate)

Status: PLANNING ONLY. No build until requirements and architecture are agreed by Brendan, Claude (coordinator), and agy.
v3 = v2 + Brendan's answers (2026-10-27) + verification of YNAB API/library. v4 = v3 + agy round 3. v5 = Brendan's answers to Q9–Q14 (2026-10-27). Pending agy final sign-off.

## 1. Goals (why)
- G1. The system (AI) maintains the YNAB budget. Brendan's involvement in YNAB upkeep is negligible.
- G2. Follow Ramit Sethi's system: pay-yourself-first flow, a 10-minute monthly check-in, attention on big wins (investing rate, income, housing).
- G3. Answer ad-hoc questions in plain English from any AI client, with correct, sourced answers ("Did I spend more this month?", "When did my paycheck land?", "When did I last pay tithing?", "How did my investments do this month?").
- G4. Take actions by conversation ("I bought three more discs today with my fun money") — the system changes transactions/categories accordingly. It NEVER moves real money.
- G5. Flag abnormal events without noise.
- G6. Let any AI model answer strategy questions (pay off the house vs. invest, early retirement) from Brendan's latest complete financial state.

## 2. Functional requirements

### Data
- R1. Mirror the full YNAB plan (accounts, category groups, categories incl. targets, payees, transactions incl. deleted, scheduled txns, months) into a local store, kept current via delta sync (`last_knowledge_of_server`).
- R2. v1: YNAB is the ONLY data source. All pertinent accounts (incl. investment/retirement/mortgage) are assumed linked in YNAB as tracking/loan accounts. Each account is tagged with a tax bucket (pre-tax / Roth / taxable / HSA) for strategy questions. Direct Wealthfront/Robinhood/other feeds: only in a later version, if needed.
- R3. (Removed by Brendan.)
- R4. Small hand-maintained profile: paycheck identification (payee/pattern), pre-tax deductions (401k %, HSA, ESPP) for savings-rate math, retirement assumptions. Mortgage lives in YNAB as a loan account (API exposes balance, interest rate, escrow).

### YNAB maintenance (the system owns the budget)
- R5. Categorize every new transaction, and the fresh-start backlog (R8c), with minimal human effort.
- R6. Auto-apply when classifier confidence is above a threshold calibrated on Brendan's previously-categorized history (mapped old→new categories) to achieve >= 85% precision (Brendan's choice: favor automation). Below threshold → ask in Slack (R9b).
- R6c. Continuous audit: the AI can re-review any or all categorized transactions at any time (on demand via MCP, and on a schedule), with the reason/confidence for each past decision available, and correct its own mistakes. Never overrides human edits (R6b).
- R6a. Explicit handling for ambiguous classes:
  - Amazon: v1 = ask in Slack when unsure. [v1.1] item-detail enrichment + split at first categorization (email receipts / order-page scraping / data export).
  - P2P (Venmo etc.), refunds/returns, split transactions.
  - Credit-card payments and on-budget transfers: never categorized.
- R6b. Edits made by Brendan or his wife directly in YNAB are ground truth: learned from, never reverted. YNAB records no editor identity, so a human edit = any field that diverges from the system's last write in its audit log; the system must not overwrite such fields.
- R7. The system may make destructive changes in YNAB: recategorize, rewrite memos, split (once — API cannot edit an existing split; correcting one = delete + recreate), approve, delete duplicates, create/rename categories and groups (API cannot hide or delete them — mistakes go to an "Archive" group; hiding is a manual UI step), set targets, and assign/move dollars between categories. It may NOT move real money (structurally impossible through the YNAB API; also no bank/brokerage credentials are given to the system). Every write is logged with before/after state so any change can be reverted (splits/deletes: by recreation). Respect the rate limit (200 req/hr; bulk PATCH).
- R7a. Write tiers. Routine (autonomous): categorize, memo, approve, flag, split a new unsplit txn, assign per R7b. Structural/destructive (require explicit confirmation in the initiating conversation, with a dry-run preview): delete transactions, create/rename/move categories or groups, move assigned dollars outside R7b rules, bulk changes over N transactions. Content from untrusted sources (emails, web pages, receipts) is never treated as instructions.
- R7b. Paycheck assignment rule: when a paycheck lands, assign in this order (approved): (1) investment/savings transfers + goal targets, (2) fixed-cost targets for the month, (3) tithing, (4) fun money (fixed amount each), (5) remainder → guilt-free or a named goal. Never exceeds Ready to Assign. Amounts set at first run.
- R8. YNAB structure follows Ramit's Conscious Spending Plan: category groups Fixed Costs / Investments / Savings / Guilt-Free, created in YNAB itself (API supports POST category_groups/categories).
- R8a. Fun money: separate categories for Brendan and his wife; "my fun money" resolves by the speaker's identity. Conversational intents ("I bought three more discs today with my fun money") are stored as pending intents and matched to imported transactions within a date window (bank lag up to ~5 days), using any amount/merchant hint given. Exactly one plausible candidate → apply. Zero or several → ask in Slack (R9b). Unmatched intents expire with a question after N days.
- R8b. Savings goals (sinking funds): trips, auto replacement, fence, etc. as categories with YNAB targets. Goals can be created/changed by conversation.
- R8c. Fresh start: Brendan triggers YNAB's Fresh Start in the UI (no API for it). The old plan stays readable (R1 mirrors both: archived plan for history/classifier calibration, active plan for live ownership). Goal categories are seeded with money already earmarked (e.g. auto-replacement money in bank savings). Fun money: compute each person's Aug-2026-to-now balance (monthly amount minus fun spending identified in the old plan) and seed it as a starting category balance. Total seeding ≤ on-budget cash; the system proposes the seeding plan and Brendan approves it once.

### Conversation, questions & reporting
- R9. Remote MCP server exposing read tools (deterministic queries: code does arithmetic and dates, model interprets; answers cite transactions and data freshness) and write tools (everything in R7). Usable from any AI client that supports remote MCP.
- R9a. Financial-state snapshot tool: balances by account and tax bucket, mortgage terms, average monthly income/spend by CSP bucket, savings/investing rate, goal progress. This is what strategy questions use (G6) — no dedicated simulator in v1.
- R9b. System-initiated questions in Slack (low-confidence categorizations, unmatched intents, unknown large transactions). Brendan's reply in Slack is applied to YNAB.
- R10. Monthly check-in in Slack: Ramit's four questions (transfers happened? investments went through? bills changed? anything unusual?), CSP percentages, goal progress, fun-money balances, 60-day test. December edition includes the +1% investing-rate nudge.
- R11. Anomaly alerts in Slack: missing/short paycheck, recurring bill price change, duplicate charge, unusually large txn vs. same-payee history, tithing behind. Budget <= ~2 non-actionable alerts/month.
- R11a. Data freshness: alert when a YNAB account's link is in error (`direct_import_in_error`) or hasn't imported in > N days; every answer states data freshness.
- R12. Tithing tracker: basis is 10% of NET pay from paychecks only (other income excluded). Tracks last payment, paychecks since, amount owed. Net pay is read from actual paycheck deposits, so it adjusts automatically as pay changes.

## 3. Non-functional requirements
- N1. Runs on hoid. Full ledger lives on hoid. Brendan accepts that query results go to the AI provider of whichever chat session uses the MCP.
- N2. Passes its own 60-day test: if ignored for 60 days nothing breaks silently; failures alert in Slack.
- N3. Secrets stored on hoid, never in chat or git.
- N4. YNAB mirror is disposable. Non-reproducible data (profile, intents, category mapping, write/audit log, any manually captured balances) has encrypted off-site backup.
- N5. An evolution of YNAB_AutoCategorizer, in Go wherever possible. Not a new app.
- N6. Reachable from anywhere Brendan has AI access. Public exposure of hoid's MCP is acceptable.
- N7. Security (because N6 + R7 = a public endpoint with write access to the family budget): strong authentication on every MCP and Slack entry point (only Brendan and his wife); audit log of every write; ability to revoke access quickly; no money-moving credentials anywhere in the system.
- N9. Two users: Brendan and his wife each have their own identity on Slack and on the MCP (installable in any of their AI apps). Every action and intent is attributed to a user.
- N8. Shared view: Brendan's wife reads YNAB directly and also uses Slack and the MCP. The YNAB budget must stay legible to a human (clear category names, notes) since she reads it directly.

## 4. Out of scope
- Moving money, paying bills, executing trades.
- Tax filing.
- RSU/option grant modeling (R3 removed).
- Dedicated strategy simulator (G6 served by R9a + the model).
- Wall/kiosk dashboard.

## 5. Decisions log
- Tithing: 10% of net pay, paychecks only. Single shared view; YNAB + Slack + MCP for both spouses. Access from anywhere; public exposure OK. Fresh start with seeding. Slack for alerts/questions. Mortgage in YNAB. Query data to AI providers accepted. Precision target 85% + continuous audit. Paycheck order = default. Amazon → v1.1. YNAB only data source in v1. Budget amounts at first run.

## 6. Verification status
Verified 2026-10-27 against the live YNAB OpenAPI spec (v1.87.0, saved at research/ynab-openapi.yaml):
- V1. API now calls budgets "plans" (`/plans/{plan_id}/...`). Old libraries/docs using "budgets" are stale.
- V2. (was U4) API can create category groups and categories (POST) and set targets (`goal_target`, `goal_target_date`, recurring NEED), and assign per-month amounts (PATCH months/{month}/categories).
- V3. (was U5) Accounts expose `direct_import_linked`, `direct_import_in_error`, `last_reconciled_at`, and loan fields (`debt_interest_rates`, `debt_escrow_amounts`, ...). R11a can use real link-error status.
- V4. (was U6) GET /plans/{id}/transactions supports `last_knowledge_of_server`. Repo pins `github.com/brunomvsouza/ynab.go v1.5.0` (lacks it). Latest is v1.6.1 (2026-07-21), a full rewrite at module path `pkg.venceslau.dev/ynab`: all 44 operations, first-class delta sync, int64 milliunits. Its vendored spec is 1.86.0, same endpoint set as live 1.87.0.
- V5. Amazon item enrichment is feasible: existing tools use email receipts, order-page scraping (Chrome extension), or Amazon's "Request My Data" export (legacy order-report tool sunset 2023).

Still unverified:
- U1. Jev: model name, pricing, latency, API shape.
- U2/U3. Moot for v1 (YNAB only).

API limits verified 2026-10-27 (agy found, Claude confirmed in spec):
- V6. Existing splits can't be modified (category_id fixed; subtransactions update returns error). Splits not allowed on tracking accounts or on-budget transfers.
- V7. No API to hide or delete categories/groups (SaveCategory/SaveCategoryGroup have no `hidden`; no DELETE).
- V8. Transactions carry no last-modified-by field.
- V9. No API to create a plan or perform Fresh Start (GET /plans only).
