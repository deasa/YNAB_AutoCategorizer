# Spike M0-5: YNAB read-only probe

Probe: `spikes/ynab/` (its own module, `pkg.venceslau.dev/ynab` v1.6.1), run against Brendan's live plan on 2026-10-27.
It sends GET requests only: its `http.RoundTripper` rejects every other method before the network. One run is
**8 requests**, and the day's total, debugging runs included, was 27. Nothing captured from the API is committed. The
counts below come from the final run.

Labels: **VERIFIED** means observed directly in this probe's live responses. **UNVERIFIED** means inferred or not
observable read-only.

## Findings

### F1. The library can't decode the live full plan as shipped (VERIFIED, blocker for M1-1)
`Plan.Export` / `Plan.Delta` on v1.6.1 fail with:
`decode response: parsing time "2025-08-28T13:55:15.492" as "2006-01-02T15:04:05Z07:00"`. The field is
`goal_snoozed_at` on a category, which YNAB sends **without a zone designator**. The library decodes it as RFC 3339
`time.Time`, so the whole response fails. In this plan one category carries the value, and it shows up twice in a full
export (in `categories` and inside `months[].categories`). The probe works around it by appending `Z` in the transport
before decoding. M1-1 needs a fix: either a library patch or upstream issue so the field becomes a lenient time type, or
the same transport shim.

### F2. Library errors include the plan id (VERIFIED)
Error strings have the form `ynab: GET plans/<plan-uuid>: decode response: …`. M1 must redact these before logging or
posting to Slack. This matters only if the plan id counts as sensitive, and the briefs treat it that way.

### F3. Full plan fetch cost (VERIFIED)
| Metric | Value |
|---|---|
| Requests | 1 (`GET /plans/{id}`) |
| Response (decoded) | ~2.56 MB |
| Wall time | 0.68–0.83 s |
| Accounts | 24 (18 open) |
| Category groups / categories | 12 / 89 (9 of them `deleted=true`) |
| Payees | 2,801 |
| Transactions / subtransactions | 2,066 / 18 |
| Months | 19 |
| Scheduled transactions | 0 |

The full fetch **omits deleted transactions, accounts and payees**, but **includes deleted categories**
(`deleted=true`), so a full fetch alone isn't a reliable list of tombstones. VERIFIED by comparing it with the
`last_knowledge_of_server=1` delta in F4.

### F4. Delta fetches (VERIFIED)
Passing `last_knowledge_of_server` (`ynab.Since(sk)`, or `Plan.Delta(&SyncState)`, which advances the cursor itself):

| Cursor | Size | Contents |
|---|---|---|
| `sk` (current) | 660 B, ~0.2 s | every collection empty; still returns the plan shell and `server_knowledge` |
| `sk-25` | 10.6 KB | 4 accts, 2 cats, 4 payees, 6 txns, 2 months |
| `sk-500` | 233 KB | 9 accts, 89 cats, 42 payees, 95 txns, 4 months |
| `1` | 2.60 MB | 31 accts, 2,838 payees, 2,121 txns of which **50 `deleted=true`**, 18 subs |

- An empty delta is cheap, so polling every 15 minutes is fine.
- **Deletes are tombstones:** the full object with `deleted: true`, still carrying id, date, approved and other fields.
  They show up only in deltas.
- `months` and `categories` can come back in a delta even when only the budget totals changed. On a busy day, a delta
  can be larger than the transaction count alone suggests.
- I couldn't observe a delta caused by a live change during the probe, because the plan didn't change between calls.
  Reading from an older cursor shows the same wire shape. UNVERIFIED: whether a delta from an older cursor is the same as
  one that accumulated in real time. That's very likely, since it's the same endpoint.

### F5. Split transactions (VERIFIED)
- **Plan endpoint (export/delta):** subtransactions come back as a **flat top-level `subtransactions` collection**. Each
  has `id, transaction_id, amount, category_id, memo, payee_id, transfer_account_id, deleted`. Stitch them to their
  parent with `transaction_id` → `transactions[].id`. There's no `payee_name` or `category_name` on this endpoint.
- **Parent:** `category_id` is **set to an id that isn't in the plan's `categories` list**. It's YNAB's internal "Split"
  sentinel. Treat a transaction that has children as split, and don't look up its category id.
- **Amounts:** the parent holds the total. In all 9 splits, the children's amounts sum exactly to the parent's.
- **Categories live on the children.** All 18 children had `category_id`, and none had `payee_id`, a transfer or a memo
  in this plan.
- **Transactions endpoint** (`GET /transactions/{id}`, and the list endpoints): children come back nested as
  `subtransactions[]` on the parent, with `category_name`. The parent's `category_name` is `"Split"`.

### F6. Account link and import fields (VERIFIED)
- Available fields: `direct_import_linked`, `direct_import_in_error`, `last_reconciled_at`. Both the plan export and
  `/accounts` return them. `/accounts` adds `*_formatted` and `*_currency` balances.
- There is **no last-import timestamp field** anywhere: not on the account, and not on the plan except the plan-level
  `last_modified_on`. Freshness per account has to be derived from the newest transaction with an `import_id` in that
  account.
- Live state: 6 accounts linked, **3 `direct_import_in_error=true`**, all of them credit cards. Their newest imported
  transactions are 342 days old, 243 days old, and nonexistent. The 3 healthy links (2 checking, 1 savings) last imported
  1–12 days ago. The R11 staleness and link-error alerts will fire on day one.

### F7. §3.1 payee memory: unapproved and already categorized? (VERIFIED counts, UNVERIFIED cause)
Imported transactions (`import_id` set, not deleted) from the last 60 days: **75**.

| Bucket | Count |
|---|---|
| Transfers | 6 |
| Splits | 0 |
| Approved | **0** |
| **Unapproved with a real category already set** | **9** |
| Unapproved with `Inflow: Ready to Assign` (all inflows) | 21 |
| Unapproved with `category_id: null` | 39 |

- Every one of the 9 "unapproved with category" has an **earlier approved transaction with the same payee and the same
  category**, which is the pattern payee memory would produce. Examples (payee → category): Freedom Mortgage →
  Home - Mortgage, Rocky Mountain Power → Home - Electric, PayPal → Discretionary.
- Of the 60 unapproved transactions with no category or only RTA, 56 have a payee that **was never approved before**, so
  there's no memory to draw on. All of them are in checking or savings. Most payee names look like person-to-person
  payments: a person's name plus a quoted memo, without "Venmo" in the payee. That's relevant because the current Venmo
  skip rule matches on the payee name and won't catch these.
- **Confound (UNVERIFIED):** the existing auto-categorizer on its 6h ticker also writes categories to unapproved
  transactions and leaves them unapproved, and since dd67de8 it also maps inflows to RTA. The API doesn't say who set a
  field, so these 9 and 21 can't be attributed to YNAB rather than our app from data alone. If the app hasn't been
  running live since late August, they're YNAB payee memory.
- No transaction approved in 60 days means that for this plan, "approved" is a strong signal of human review, but it's
  rare. The labeled-example stream (§3.1 step 2b) will be thin until Brendan approves in bulk.

### F8. Rate limiting (VERIFIED)
None of the 8 responses had an `X-Rate-Limit` header, and v1.6.1 exposes no rate-limit accessor, only
`ErrRateLimited` on 429. Usage can't be read from responses, so M1 has to count its own requests against the
documented 200/hour per token.

### F9. Raw shapes beyond the library structs (VERIFIED)
Raw JSON keys on accounts, transactions and subtransactions match v1.6.1's `*Base` structs exactly. Nothing in the
plan-export payload is dropped silently. The transaction list endpoint adds `account_name`, `payee_name`,
`category_name`, `flag_name`, `amount_formatted`/`amount_currency` and nested `subtransactions`.

## Recommendation
Adopt `pkg.venceslau.dev/ynab` v1.6.1 for M1-1 and **use `Plan.Delta(&SyncState)` as the only sync read**: one request
per 15-minute tick, ~0.2 s and under 1 KB when idle. Persist `SyncState` in `sync_state`. For the initial load, take
the full export, then switch to deltas. Before M1-1 ships, fix F1: either patch or upstream the lenient
`goal_snoozed_at` parse, or install the zone-normalizing transport. Wrap every library error to redact the plan id
(F2).

## Impact on architecture
- **§3.1 step 1 (delta, tombstones):** confirmed as designed. Tombstones show up only in deltas, and deleted categories
  also appear in full fetches, so the upsert path has to honor `deleted` in both. `MergeByID` does that.
- **§2 data model:** `subtransactions` keyed by `id` with FK `transaction_id`. The split parent's `category_id` is a
  sentinel that isn't in `categories`, so either don't enforce an FK from transactions to categories or map the sentinel
  to NULL plus an `is_split` flag. Category lookup and categorization should skip split parents (or work per child).
- **§3.1 step 4 (freshness):** there's no API import timestamp. Compute per-account freshness as
  `max(date) where import_id is not null`. That's at date granularity, which is coarse; consider also recording the
  local `synced_at` when a new `import_id` first appears.
- **§3.1 step 3 (link errors):** `direct_import_in_error` works and is currently true on 3 cards. Expect alerts to fire
  immediately. Dedupe them so the first sync doesn't spam Slack.
- **§3.1 step 2 (payee memory):** the assumption is plausible. 9 out of 9 unapproved-and-categorized transactions match
  a prior approved payee→category. But it can't be attributed to YNAB until the old categorizer is confirmed off, so
  keep the UNVERIFIED tag and resolve it in M1 by observing new imports while the old app is stopped. Also: an unapproved
  transaction already in `Inflow: Ready to Assign` should count as pre-categorized for skip logic.
- **Rate budget:** there's no header to read, so add a local token-bucket counter. One delta every 15 minutes, plus
  imports and writes, stays far under 200/hr.
- **P2P payees:** most of the uncategorized backlog is person-to-person payees without "Venmo" in the name. The R-level
  Venmo/P2P skip rule needs a better detector than a substring match. This feeds into §3.2.
