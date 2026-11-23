# M3-jevctx: what Jev sees, and a Jev-ready category list

Collected 2026-11-23 (Denver). Read-only: the code, a fresh copy of hoid's DB (the `hoid_check.sh` copy loop,
`PRAGMA quick_check` ok), the M3-dryrun and M3-simplify reports, the Jev spike (`docs/build/spikes/jev.md`) and
Ramit Sethi's own pages. No Jev calls, no YNAB calls, nothing written anywhere. DB counts below are from that copy.
"Window" means the 93 current model decisions (hashes `451fbae6`/`74884ed1`, 2026-11-12 to 11-21).

## Top holes (ranked by expected impact)

1. **Production Jev gets no payee history, but calibration measured Jev with it.** In the window,
   `payee_previous_categories` was sent for **0 of 93** transactions. hoid's `plan_scan` table is empty, so the label
   plans are the active plan alone, which has no usable labels. `finbot calibrate` ran with the archived plan's
   history. In the spike, history was worth **+21 points of top-1** (batch10: 48% → 69%), and without it no τ reached
   85% precision. Had the archived plan been in the label plans, 54 of the 93 would have had history. Only 29 of those
   would keep it once `Discretionary` is retired, because history keeps only labels that are current candidates
   (`classify/rules.go:72`).
2. **Some transactions have no right answer, so Jev picks the nearest junk.** Retired's `Discretionary`,
   `Talk About` and `Needs a job!` are still candidates (Retired isn't hidden yet, BA-25). Income inflows reach the
   model too: the guard skips Ready to Assign only when YNAB already set it (`classify/guard.go:46`). 8 of the 93 were
   inflows, and 4 of them (two payroll payees) got `Needs a job!`. A Choice always picks something. Nothing tells
   code "none of these fit".
3. **Descriptions are missing, or they're YNAB budget notes.** Of 45 candidates today, 6 have a policy description,
   11 fall back to their YNAB note (`classify/candidates.go:61-63`), and **28 have none**. The notes are budgeting
   scratch, not descriptions: dated reminders, a to-do list, "Do we have this anymore?", and an **income figure**,
   which goes to Jev on every request. The near neighbours behind the dry-run misses have no description that tells
   them apart: Voice Lessons vs Kids Classes (4 confusions), and Subs - Amazon Prime vs Shopping (2).
4. **The payee alone can't settle the category for multi-purpose stores, and nothing else helps.** 0 of 529
   active-plan transactions have a memo, so Jev sees only payee, amount, date and account. Amazon is 10 of 93, and
   Walmart, Costco, Smith's and Harmons sell both groceries and household goods. Harmons also sells fuel: one went to
   Gas & Transport. This fails the skill's human test: a person couldn't tell either. Merging (option A) and asking
   are the fixes. Better wording isn't.
5. **The two fun categories and set-aside labels leak.** Fun - Brendan / Fun - Hilarie Under The Radar are still
   candidates (1 window decision picked one), against the 2026-11-22 decision. Set-aside and fun labels counted as 2
   of the 8 would-apply misses (M3-simplify).
6. **Account names are opaque.** Jev gets `B Freedom Unlimited`, `BH … Venmo`, `United Biz`. It isn't told that
   `B`/`H` mean whose card, that `Biz` cards and `Bike Business – 2233` are business accounts (Reimbursement,
   Business Bike), or what each card is used for. 61 of 93 came from one card, so the account says little for most
   rows. It matters most for Reimbursement and the business accounts.
7. **No amount or recurrence facts.** The amount is sent, but not "this payee is usually $X" or "this payee charges
   monthly". Code already knows both (`alerts` computes trailing medians). Lower impact: payee rules already handle
   recurring bills once history flows.
8. **Smaller ones.** Batching 10 transactions per state hurt without history in the spike (single 58% vs batch10
   48% top-1) and was neutral with history (67% vs 69%). Calibration calls `Guard` without the account type
   (`calibrate/calibrate.go:223`), so card-payment detection differs slightly from runtime.

## Decisions Brendan needs to make

1. **Turn history on in production:** let sup run `finbot sync --archived` once on hoid (YNAB GETs only, fills
   `plan_scan`). Until then, every number from dry run and calibration describes a different input.
2. **Approve the final list in §4:** option A plus four exclusions (`Buffer`, `Wise - Buffer`, `Auto - Replacement`,
   and `Business Bike`, unless it gets spent from often). Correct the descriptions marked *(confirm)*.
3. **Church payments:** if tithing and fast offerings go to the same payee in one payment, Jev can't tell them apart
   (the guard skips split payments; a single payment carries only one amount). Should finbot always ask for them?
4. **Stop sending YNAB notes to Jev:** either give every target a policy description (no code), or remove the
   fallback to notes (a small code change). Recommendation: do both.
5. **Labelling interface:** a CLI on the Mac (recommended), or a local web page if Hilarie will label too (§5).

## 1. Ramit Sethi's Conscious Spending Plan

The four buckets are Ramit's, as percentages of take-home pay:

| bucket | target | what goes in it (his examples) |
|---|---|---|
| Fixed costs | 50-60% (65-70% in costly cities) | rent or mortgage, utilities, insurance, groceries, transportation, cell phone, subscriptions and memberships, debt payments, plus a **15% buffer** for surprises (the workbook calls it "Other fixed costs") |
| Investments | ~10% | 401(k) with the match, Roth IRA, low-cost index funds |
| Savings | 5-10% | emergency fund (3-6 months) first, then named goals: vacation, down payment, wedding, gifts, a "fun fund" |
| Guilt-free spending | 20-35% | dining out, shopping, hobbies, entertainment, the daily coffee, concerts, travel, clothes |

- **Guilt-free money** "is meant to be spent", with no guilt and no tracking, as long as the other buckets are
  handled first. His pages say nothing about splitting it per person. **His-and-hers fun money is a common
  adaptation** (and Brendan's: two Under The Radar buckets, settled at the monthly meeting, never per transaction).
- **Savings vs investments:** savings are short-term goals and emergencies you'll spend. Investments are long-term
  retirement money you won't touch. Irregular costs (annual subscriptions, quarterly insurance, holiday gifts) are
  divided by 12 and saved monthly. YNAB's "true expenses" funds (Auto Repairs, Medical) are this idea put into YNAB,
  an adaptation.
- **Review:** a regular check-in, weekly or monthly, to troubleshoot. A monthly 20-minute "money date" or
  "Monthly Money Meeting", a quarterly audit of fixed costs, a yearly review with his wife of a few hours, and
  revisiting the percentages after a raise, a move or a new child. R10's "Ramit's four questions" and "60-day test"
  don't appear on these pages. They may come from the book or a course: check before R10 quotes them as his.
- **Granularity:** Ramit's plan is deliberately coarse (four buckets, few lines). Fine-grained fixed costs are a YNAB
  habit, not his. That backs the 2026-11-22 decision to merge near neighbours.

Sources: <https://www.iwillteachyoutoberich.com/conscious-spending-basics/>,
<https://www.iwillteachyoutoberich.com/how-to-budget/>,
<https://www.iwillteachyoutoberich.com/how-to-calculate-expenses/> (all updated December 2024), and the book *I Will
Teach You to Be Rich* (2nd ed., 2019), ch. 4. I didn't re-read the book for this report: the 15% "Other fixed costs"
line comes from secondary coverage of its workbook.

## 2. What Jev sees today

Path: `Pipeline.Run` → `run.decide` (`classify/pipeline.go:342`) → Guard → payee rule → `Model.Classify`
(`pipeline.go:453`) → `jev.Client.Classify` → `buildRequest` (`classify/jev/jev.go:177`).

- **Candidates** (`classify/candidates.go:44`): live categories minus hidden, internal, Credit Card Payments and
  set-asides (whole-word keyword match, `classify/policy.go:78`). There are 45 today (matching M3-simplify). Retired's three are still in.
- **Criteria** (`jev.go:154`): each candidate name maps to `"Group: <group>. <description>"`. The description is the
  `policy.json` override, else the YNAB note (`candidates.go:61-63`), else nothing.
- **State per transaction** (`jev.go:144`, `:177-195`): payee (YNAB's payee name, else the imported one,
  `rules.go` `PayeeName`), amount with direction (`money`, `jev.go:198`), the date with its weekday, the account name,
  the memo (omitted when empty), and `payee_previous_categories`: the payee's last 5 human labels, newest first, kept
  only if they're current candidates (`rules.go:66-72`, `HistoryLen` = 5), and omitted when there are none.
- **Questions:** one `choice` per transaction, all sharing the 10-transaction state (`DefaultBatchSize`, `jev.go:33`),
  with the fixed text at `jev.go:126`.

Request shape (fake values):

```json
{
  "model": "jev-1.14.0",
  "state": {"transactions": {
    "t01": {"payee": "Example Grocer", "amount": "$84.12 outflow (money spent)",
            "date": "2026-11-14 (Saturday)", "account": "B Example Card",
            "payee_previous_categories": ["Groceries", "Groceries", "Household & Kids"]},
    "t02": {"payee": "Example Payroll", "amount": "$1500.00 inflow (money received)",
            "date": "2026-11-15 (Sunday)", "account": "Example Checking"}
  }},
  "questions": {
    "t01": {"type": "choice",
            "instructions": "Which budget category does the transaction in `transactions.t01` belong to?",
            "criteria": {
              "Groceries": "Group: Fixed Costs. Food and drink from grocery stores and supermarkets, for eating at home. …",
              "Home - Cell Phone": "Group: Fixed Costs. NOV 2018: example budget note …",
              "Buffer": "Group: Savings Goals.",
              "…": "… one entry per candidate, the same map in every question"}},
    "t02": {"…": "same, for t02"}
  }
}
```

Response (`jev.go:236-277`): `{"model": "jev-1.14.0", "answers": {"t01": {"type": "choice", "choice": "Groceries",
"probabilities": {"Groceries": 0.83, "Household & Kids": 0.11, …}}}}`. A different model version, a missing answer, a
choice or probability outside the candidates, or probabilities summing outside 0.9-1.1 fail the whole batch.
`Prediction.Top` is the names ranked by probability. The pipeline gates on p_top ≥ τ (default 0.8,
`pipeline.go:282-284`), not on Jev's `confidence`. That matches the skill's guidance. In the window, the mean p_top
was 0.79.

## 3. Holes, with evidence

See the ranked list at the top. Each item cites its evidence: code line, report number or DB count. Two more
observations:

- **The question wording is fine.** It names the subject, asks one judgment, and refers to the state by path. The
  gains are in the state (history, descriptions, account facts) and in a null-case question. Rewording the question
  to fix particular misses would be the skill's "prompt gymnastics".
- **History uses old names.** 659 of 1048 history labels are `Discretionary` (M3-simplify §4), and its payees are
  grocery stores, fast food and Amazon. After the restructure, those labels vanish from `payee_previous_categories`
  for exactly the payees that are hardest.

## 4. Proposal: the final list and what Jev gets

Start from M3-simplify option A (`deploy/restructure/csp-2026-11b.json`). **Targets** are what Jev may answer.
Descriptions are one line each, for `policy.json`. *(confirm)* marks a guess about what the category covers.

**Fixed Costs (25 targets)**

| category | Jev description |
|---|---|
| Home - Mortgage | The monthly mortgage payment. |
| Home - Highland City Utilities | The city utility bill: water, sewer and garbage. *(confirm)* |
| Home - Electric | The electric company's bill. |
| Home - Natural Gas | The natural gas company's bill. |
| Home - Internet | Home internet service. |
| Home - Cell Phone | The mobile phone bill. |
| Home - Insurance | Homeowners insurance premiums. *(confirm)* |
| Auto - Insurance | Car insurance premiums. |
| Home - Lawn Service | Lawn and yard care service. |
| Gas & Transport | Fuel, parking, tolls, rideshare and transit fares. |
| Groceries & Household | Grocery stores, supermarkets and warehouse clubs, and household supplies and kids' everyday needs from any store. Restaurants and takeout are Guilt-Free Spending. |
| Home - Kids Classes & Education | Lessons, classes, tutoring and school fees for the kids, music and voice lessons included. |
| Larks Dues | The quarterly Larks dues. *(confirm)* |
| Reimbursement | Work or other expenses someone will pay back, such as work travel or purchases made for someone else. |
| Subs - Amazon Prime | Only the Amazon Prime membership fee. Amazon orders are Guilt-Free Spending. |
| Subs - YNAB, Canva, Spotify, Google Domains Website, Disney+, TrainerRoad, Bark, GoPro, Point.Me, Google One | "The <service> subscription charge." (one each, 10 lines) |

**Giving (2 targets):** Church - Tithing ("Tithing paid to the church"), Church - Fast Offerings ("Fast offerings
paid to the church"). See decision 3. Giving - Generosity stays a set-aside.

**Investments (0 targets):** Retirement, Stock Investments, Mortgage Payoff stay set-asides.

**Savings Goals (5 targets):**

| category | Jev description |
|---|---|
| Auto - Repairs and Expenses | Car repairs, maintenance, tires, registration and inspections. |
| Home - Improvement / Maintenance | Repairs, tools, materials and contractors for the house and yard, hardware stores included. |
| Home - Tech / Computer Replacement | Replacing computers, phones and other household tech. |
| Wise - Medical & Dental | Doctor, dentist, lab, pharmacy and other medical bills. |
| Wise - Emergency Preparedness | Emergency supplies: food storage, 72-hour kits, water storage and backup power. |

**Excluded from candidates** (a person can still pick them in Slack): Birthday ×6, Holiday ×2, Fun - Next Trip
(set-asides, as today); **`Buffer` and `Wise - Buffer`** (money held, never spent from: a new set-aside keyword
`Buffer`); **`Auto - Replacement`** (a rare one-off that Brendan categorizes himself); **`Business Bike`** unless
Brendan says it's spent from often *(decision 2)*.

**Guilt-Free (1 target):** Guilt-Free Spending: "Wants: restaurants, fast food, takeout, coffee and treats; Amazon
orders and general shopping; hobbies, entertainment, clothes for fun, and gifts not paid from a birthday or holiday
fund." Both fun categories are excluded (set-aside keyword `Radar`).

That's **33 targets** (34 if Business Bike stays a target), against 45 today and 37 in M3-simplify.

**Differences from `csp-2026-11b.json`:** none in YNAB structure. The spec stays as it is. The differences are
classifier-side: `Buffer`, `Wise - Buffer` and `Auto - Replacement` (and maybe `Business Bike`) leave the candidates,
and every target gets a description.

**Policy/data only (no code):**
- Descriptions for all targets (above), which also replaces every YNAB note in the request.
- Set-aside keywords `Radar` and `Buffer`.
- Hide Retired (BA-25), and run the restructure (BA-27).
- Run `finbot sync --archived` on hoid (decision 1).

**Needs code:**
- `Needs a job!` treated as internal (M3-simplify §5).
- A label alias map for history and calibration (M3-simplify §4), plus the manual labels (§5).
- A `never_target` policy list, if Brendan prefers it to stretching "set-aside" (needed for `Auto - Replacement`,
  since a `Replacement` keyword would also catch Home - Tech / Computer Replacement).
- Drop the YNAB-note fallback.
- **A companion Noul per transaction in the same request** (the skill's null-case pattern): "Does one of the listed
  budget categories fit the transaction in `transactions.t01`?" True: "a purchase, bill, refund or donation one of the
  categories covers". False: "income such as a paycheck, or money moved between the household's own accounts". In
  code, below 0.3 means always ask; it never applies. It needs a live probe and its own place in calibration.
- **Account facts:** a policy map from account name to a short description, sent as `account_description` (for
  example "Brendan's personal card" or "business card: work travel, reimbursed"). Probe it before adopting it.
- Later, only if misses show the need: `payee_usual_amount`/`recurs_monthly` facts.
- Keep batches of 10 once history flows. Recheck single vs batch with ≤3 `JEV_LIVE` probes when the new list is live.

## 5. Labelling session plan

**Goal:** about 300 human labels against the §4 list, so the tuning 70% (the oldest rows, `calibrate.go:28-29`)
holds ≥200 model-band rows, with every target covered.

- **Pool:** every non-deleted transaction in the active and archived plans. Drop guarded rows (transfers, card
  payments, splits, P2P) using the same `Guard` and `route` calibration uses. Drop rows a unanimous payee rule would
  decide after the alias map: rules already measured 99.7% (M3-dryrun), so they don't need labels.
- **Sampling:** start with rows whose old label doesn't map (Discretionary, Talk About, the merged categories), then
  unapproved active-plan rows (the most current payees). Cap each normalized payee at 3 rows (Amazon at 10: it's the
  hard case). Fill until each target that has history has ≥5 rows, then spread across dates so both the tuning and
  holdout spans are covered. The rows are shown in random order, and **never with Jev's guess**, so the labels don't
  inherit its bias. Show the old YNAB category instead, as context.
- **Interface:** `finbot label` on the Mac against a DB copy. One screen per transaction:

  ```
  [ 37/300 ]  2025-03-14 (Fri)  Example Store  $42.17 outflow  B Example Card   was: Discretionary
   1 Groceries & Household   2 Guilt-Free Spending   3 Gas & Transport   …   (/ search, s skip, n no category, u undo)
  ```

  Number keys or type-ahead pick a category. `n` marks a row with no category (income or a transfer), and it's left
  out of calibration. It resumes where it stopped. A local web page does the same job and is better if Hilarie labels
  too. Slack is the worst fit: the open-question cap and channel noise, and it's slower per row.
- **Storage:** a new table `manual_labels` (txn id, import id, date, amount, payee, category name, labeller,
  timestamp, list version). It stores the row's fields so labels survive a DB refresh. `LabeledHistory` takes a
  manual label in place of YNAB's category for the same transaction. That way the labels feed calibration, the payee
  rules and `payee_previous_categories`. `finbot labels export|import` moves them to hoid. Nothing goes to YNAB.
- **Time:** about 6-8 s per row with the old category as a hint, so **35-45 minutes** for 300. Two sittings is fine.
- **Order:** label after Brendan approves the list (the names in the spec are enough; YNAB needn't be restructured
  yet). Calibrate only after the restructure and the policy deploy, since the hashes must match the live plan.
- **Code needed:** the migration and store functions, the sampler (reusing `calibrate.route`), the prompt loop,
  `LabeledHistory` plus the alias map, and export/import. About one builder task plus a review.

## Method notes

- The candidate and description counts are a query on the DB copy's categories table that applies the `Candidates`
  rules (45 candidates, 11 with a YNAB note), and they match M3-simplify.
- "Would have had history": the window payee matches (case-folded) an approved, categorized, non-transfer row in the
  archived plan. This is an approximation of `NormalizePayee`.
- No live Jev probes were run. The companion Noul and the account facts are untested proposals.
