# M3-simplify: fewer, broader categories

Collected 2026-11-22 (Denver). Read-only: a fresh copy of hoid's DB (the `hoid_check.sh` copy loop, `PRAGMA
quick_check` ok), and one GET for the `finbot restructure` dry run. Nothing was written to YNAB, the sandbox or hoid.
No calibration was run.

Inputs: Brendan's decisions of 2026-11-22 (one shared Guilt-Free category; the two fun categories are never a
classifier answer and are settled at the monthly meeting; Fixed Costs stay granular except near neighbours the model
confuses) and `docs/build/reports/M3-dryrun.md`.

## Summary

- **The proposal:** one `Guilt-Free Spending` category (Shopping renamed, Dining Out merged into it), Voice Lessons
  merged into Kids Classes, Groceries and Household & Kids merged into `Groceries & Household` (option A; option B
  below keeps them apart), Larks Dues moved to Fixed Costs, and Next Trip and Business Bike moved to Savings Goals.
  Subscriptions stay granular. The model's answer list drops from 45 categories to 37.
- **Replay:** on the same 39 judged would-apply decisions, precision goes from **79.5% to 89.7%** with option A, or
  **84.6%** with option B. That's 4 or 2 more right answers. **n = 39 is far too small to confirm 85%**: no row's
  Wilson lower bound reaches 85%. At about 90%, that takes roughly 200 judged would-apply answers, about 7 weeks at
  this window's rate.
- **Calibration will break without an alias map.** Once the old categories are hidden, 818 of the 1048 history labels
  become hard negatives (labels with no right answer). 659 of them are `Discretionary`. The remaining labels can't
  meet the 200 tuning model-band rows that `auto_eligible` needs. Calibration needs an alias map (below), and probably
  the Slack dry-run answers as a second source of labels.
- **Spec:** `deploy/restructure/csp-2026-11b.json` parses and resolves against the live plan: **10 changes, 0
  unresolved, 0 warnings.** Merging means "move it to Retired, then hide Retired". The spec can't move past
  transactions or hide anything; those steps are by hand in YNAB.

## Scope 1: what's there now

Counts are from the DB copy. **History labels** follow `store.LabeledHistory` semantics: approved, really categorized,
not a split or transfer, finbot writes only with a lock, de-duplicated across plans. They come from both mirrored
plans, the active one and the archived one, 1048 rows in all. Almost all of them are from the archived plan: the
active plan has 21 approved rows, and none of them is a usable label. **Dry-run labels** are the current-hash
(`74884ed1`/`451fbae6`) would-apply and asked decisions that have a later human label, judged as
`calibrate.BuildDryRunReport` judges them: 64 judged, 39 would-apply and 25 asked. The history labels use each plan's
own names, so a category renamed since the archived plan (`Google One`, `Subs - Lawn`) shows 0 here and its old name
counts as a hard negative.

Dry-run confusions (current hashes; predicted → label):

| outcome | predicted → label | n |
|---|---|---|
| would-apply miss | Groceries → Household & Kids | 2 |
| would-apply miss | Voice Lessons → Home - Kids Classes & Education | 2 |
| would-apply miss | Groceries → Dining Out | 1 |
| would-apply miss | Subs - Amazon Prime → Shopping | 1 |
| would-apply miss | Shopping → Giving - Generosity (a set-aside) | 1 |
| would-apply miss | Household & Kids → Fun - Brendan Under The Radar | 1 |
| asked, top-1 wrong | Household & Kids → Shopping | 2 |
| asked, top-1 wrong | Shopping → Dining Out; Shopping → Home - Improvement / Maintenance | 1 each |
| asked, top-1 wrong | Voice Lessons → Kids Classes; Household & Kids → Kids Classes; Gas & Transport → Groceries | 1 each |
| asked, top-1 wrong | → Talk About (from Auto - Repairs, Reimbursement, Shopping, Subs - Amazon Prime, Subs - Disney+) | 5 |

The dry-run report shows 2 misses as "(not in the mirror)". Those labels are set-aside or fun categories the answer
options didn't carry; this analysis looked them up by name. The counts are unchanged.

## Scope 2: the proposed list

Groups follow the Conscious Spending Plan: Fixed Costs, Investments, Savings Goals, Guilt-Free, plus Giving. "Target"
means the model may answer with it. Set-asides and the fun categories are budget buckets the model never picks.

### Fixed Costs

| category | history labels | dry-run labels | action |
|---|---|---|---|
| Home - Mortgage | 9 | 0 | keep |
| Home - Highland City Utilities | 9 | 0 | keep |
| Home - Electric | 8 | 0 | keep |
| Home - Natural Gas | 9 | 1 | keep |
| Home - Internet | 7 | 1 | keep |
| Home - Cell Phone | 1 | 0 | keep |
| Home - Insurance | 0 | 0 | keep |
| Auto - Insurance | 2 | 0 | keep |
| Home - Lawn Service | 0 (+5 as `Subs - Lawn`) | 0 | keep |
| Gas & Transport | 0 | 0 | keep |
| Groceries | 0 | 10 | **option A: rename to `Groceries & Household`** (option B: keep) |
| Household & Kids | 0 | 3 | **option A: merge into Groceries & Household** (hide) (option B: keep) |
| Home - Kids Classes & Education | 27 | 5 | keep; **takes Voice Lessons**; target $400 → $810 a month |
| Larks Dues (was `Fun - Larks Dues`) | 3 | 0 | **move here from Guilt-Free**, drop the `Fun -` prefix: fixed quarterly dues |
| Reimbursement | 0 | 0 | keep |
| Subs - Amazon Prime | 1 | 0 | keep; add a description (Prime fee only; Amazon orders are Guilt-Free) |
| Subs - YNAB, Canva, Spotify, Google Domains Website, Disney+, TrainerRoad, Bark, GoPro, Point.Me | 11, 9, 8, 4, 3, 2, 1, 1, 1 | 0 | keep |
| Subs - Google One | 0 (+8 as `Google One`) | 1 | keep |

**Subscriptions stay granular.** Each one is one recurring payee, which is the job payee rules do (3 unanimous labels).
Folding them into one `Subscriptions` category changes nothing in the replay (the only subscription miss is Amazon
Prime vs Shopping, which a merge of the subs doesn't fix) and would lose the per-subscription targets.

**Groceries vs Household & Kids, both options:**

| | would-apply precision (n = 39) | asked top-3 (n = 25) | what it costs |
|---|---|---|---|
| A: merged into `Groceries & Household` | 35/39 = 89.7% | 18/25 = 72.0% | one budget line for food and household supplies |
| B: kept apart, with a rule | 33/39 = 84.6% | 18/25 = 72.0% | the 2 Groceries → Household & Kids misses stay until rules learn the payees |

The option B rule, as a one-line policy description: *"Groceries: anything bought at a grocery store, supermarket or
warehouse club, whatever's in the cart. Household & Kids: household and kids' goods from other stores."* The rule
decides by payee, so payee rules can learn it, which a rule about the contents of the cart can't.
**Recommendation: A.** It is Brendan's own rule for confused near neighbours, and the same stores (Costco, Walmart,
Smith's) sell both. The spec implements A. For B, drop the Groceries `rename+move` and the Household & Kids `move` to
Retired, and add both as `stay` in Fixed Costs (8 changes instead of 10).

### Giving

| category | history | dry-run | action |
|---|---|---|---|
| Church - Tithing | 0 | 0 | keep (target) |
| Church - Fast Offerings | 0 | 0 | keep (target) |
| Giving - Generosity | 0 | 1 | keep (set-aside, keyword `Giving`) |

### Investments

| category | action |
|---|---|
| Retirement, Stock Investments, Mortgage Payoff | keep; set-asides (keywords `Retirement`, `Investment`, `Payoff`) |

### Savings Goals

| category | history | dry-run | action |
|---|---|---|---|
| Auto - Repairs and Expenses | 43 | 0 | keep (target) |
| Home - Improvement / Maintenance | 31 | 2 | keep (target) |
| Wise - Medical & Dental | 30 | 4 (4/4 right) | keep (target) |
| Home - Tech / Computer Replacement | 6 | 0 | keep (target) |
| Auto - Replacement, Buffer, Wise - Buffer, Wise - Emergency Preparedness | 0 | 0 | keep (target) |
| Fun - Next Trip | 0 | 0 | **move here from Guilt-Free**; already a set-aside (`Trip`) |
| Business Bike | 5 | 0 | **move here from Guilt-Free**: a fund for one purchase. The rename only drops a trailing space from the current name |
| Birthday - × 6, Holiday - Christmas, Holiday - Halloween | 0 | 0 | keep; set-asides |

Savings goals stay as they are: the event funds stay set-asides, and the true-expense funds stay targets, since
spending from them is real spending.

### Guilt-Free

| category | history | dry-run | action |
|---|---|---|---|
| Guilt-Free Spending (was Shopping) | 0 | 12 | **rename Shopping**: the one shared category for dining out, shopping, entertainment and the like (target) |
| Dining Out | 0 | 15 | **merge into Guilt-Free Spending** (hide) |
| Voice Lessons | 3 | 1 | **merge into Home - Kids Classes & Education** (hide); the model confused them 3 times out of 4 |
| Fun - Brendan Under The Radar | 5 | 0 | keep as a budget bucket; **never a classifier target** |
| Fun - Hilarie Under The Radar | 2 | 0 | keep as a budget bucket; **never a classifier target** |

After the moves, Guilt-Free holds `Guilt-Free Spending` and the two fun buckets (plus the already-hidden England 2025
and San Diego Spring Break 2023). Fun-money purchases are categorized like any other discretionary spending, usually
to Guilt-Free Spending, and the monthly meeting moves budget between the buckets.

### Retired (hide the whole group)

Discretionary (659 history labels), Talk About (12), Needs a job! (0), Home - Riverton City Utilities (already hidden),
plus the three merged ones the spec moves here: Dining Out, Household & Kids, Voice Lessons.

Classifier targets go from 45 to 37 (Retired's 3 hidden, Dining Out, Household & Kids, Voice Lessons, and the two fun
categories, while Larks Dues and Business Bike stay targets). Neither fun category appears as a classifier target
anywhere in this proposal.

## Scope 3: replay

Method: each judged current-hash decision's prediction and label are both mapped through the merge map, then scored
the same way `BuildDryRunReport` scores them. For top-3, the mapped top list is de-duplicated first. The baseline
matches the M3-dryrun report exactly (31/39, and 64/79 with superseded decisions).

| map | would-apply precision | Wilson 95% | asked top-3 |
|---|---|---|---|
| current categories | 31/39 = 79.5% | 64.5% to 89.2% | 16/25 = 64.0% |
| B: Guilt-Free + Voice Lessons merges, Groceries apart | 33/39 = 84.6% | 70.3% to 92.8% | 18/25 = 72.0% |
| **A: B + Groceries & Household (the spec)** | **35/39 = 89.7%** | **76.4% to 95.9%** | **18/25 = 72.0%** |
| A + all subs merged into one | 35/39 = 89.7% | 76.4% to 95.9% | 18/25 = 72.0% |

With superseded decisions too (n = 79): 64/79 = 81.0% → B 68/79 = 86.1% (76.8% to 92.0%) → A 70/79 = 88.6% (79.7% to
93.9%).

The 4 misses left under A: Shopping → Giving - Generosity (a set-aside label), Subs - Amazon Prime → Guilt-Free
(the description fix targets this one), Groceries & Household → Fun - Brendan (finbot may never answer fun, so it
can't win this one), and Groceries & Household → Guilt-Free (Groceries → Dining Out).

**How much this proves:** not much. n = 39, and the gain is 4 transactions. A Wilson lower bound of 85% needs about
208 judged would-apply answers at 89.7%, 92 at 92%, or 45 at 95%. The replay also keeps each decision's outcome: a
merge adds the merged categories' probability together, so some asked decisions would have crossed τ, and the model
would have ranked a shorter list differently. Read it as a direction, not a measurement. The real measurement is a
dry run under the new hashes.

## Scope 4: calibration impact

`calibrate` judges a label outside the candidates as a hard negative: no answer is right. Hidden categories leave the
candidates, and the archived plan's labels keep their old names. Of 1048 history labels:

| | labels | today | after the proposal (no alias map) |
|---|---|---|---|
| Discretionary | 659 | target | hard negative |
| England 2025 (hidden trip fund) | 123 | hard negative | hard negative |
| Talk About | 12 | target | hard negative |
| `Google One`, `Subs - Lawn` (old names) | 8 + 5 | hard negative | hard negative |
| `Fun - Larks Dues`, `Business Bike ` (renamed by the spec) | 3 + 5 | target | hard negative |
| Voice Lessons | 3 | target | hard negative |
| Fun - Brendan / Hilarie | 5 + 2 | target | dropped, if they become set-asides (scope 5) |
| everything else | 223 | target | target |
| **hard negatives** | | **136** | **818 of 1041** |

That would sink calibration: about 79% of the rows could never be right. This is already true of the M3-dryrun plan
("hide Retired, then rerun calibrate"), since hiding Retired alone makes 671 more labels hard negatives.

**Yes, calibration needs a label alias map** (old name → new name, or → dropped). It should be applied to
`LabeledHistory`'s rows before `InActiveList` is set, in both `calibrate.Inputs` and the runtime's rules history, and
hashed into the policy hash:

- **rename:** `Google One` → `Subs - Google One` (8), `Subs - Lawn` → `Home - Lawn Service` (5), `Voice Lessons` →
  `Home - Kids Classes & Education` (3), `Fun - Larks Dues` → `Larks Dues` (3), `Business Bike ` → `Business Bike`
  (5). Under option A, `Groceries`/`Household & Kids` → `Groceries & Household`, and `Shopping`/`Dining Out` →
  `Guilt-Free Spending` (0 history labels each today, but the active plan's labels will build up under the old
  names until the restructure).
- **drop**, as set-asides are dropped today: `Discretionary` (659). It's a catch-all: its top payees are grocery stores,
  fast food, Amazon, Costco and streaming, so it maps to no single new category. Also drop `Talk About` (12) and
  `England 2025` (123).

With the map: 24 labels are renamed, 794 dropped, 0 hard negatives, and about **247 usable labels**. That's about 173
tuning rows before guardrails and payee rules take theirs, so the **200 tuning model-band rows `auto_eligible` needs
are out of reach from history alone**. Two more label sources would close the gap:
1. Answered `dry_run_check` questions (95 so far) as labels. They are human labels, but nothing writes them to YNAB,
   so `LabeledHistory` never sees them.
2. Brendan approving categorized transactions in YNAB. The active plan has only 21 approved rows today.

Also: **hoid's DB has no `plan_scan` rows**, so on hoid `LabelPlans` is the active plan alone, and the runtime's payee
rules have no history to learn from. That fits "no payee rule fired in the window" in M3-dryrun. Wherever `finbot
calibrate` runs, check that `finbot sync --archived` has populated the scan, or the archived plan's labels are left
out.

## Scope 5: policy changes needed (not built)

**The two fun categories leave the classifier.** Two ways:

1. **As set-asides in `classify/policy.json`**: add the keyword `Radar` to `set_aside_keywords`. Matching is by whole
   word, and only the two fun categories contain it. No code change. `Candidates` drops them, `LabeledHistory`
   excludes their labels (7 dropped, not hard negatives), and the payee rules never learn them. They stay `Eligible`,
   so a person can still pick them in Slack's "other…" list. The catch: "set-aside" stretches to cover something that
   isn't an event fund.
2. **By exclusion in `Candidates`**, for example a policy field `never_target: [...]`. Unless the same names also go
   to `LabeledHistory`'s `excludedNames`, their 7 labels become hard negatives, which punishes the model for an answer
   it isn't allowed to give. It needs a code change.

**Recommendation:** option 1 now. If the semantics matter, option 2 later as a policy field that feeds the same
`SetAsides` list. Either way the policy hash changes, so it belongs in the same recalibration.

**`Needs a job!`:** add it to `internalNames` in `classify/candidates.go`, which removes it from `Candidates` and from
`Eligible` (Slack). Also exclude it in `LabeledHistory`, next to Ready to Assign, since YNAB doesn't flag it as
internal. Hiding Retired removes it from today's plan, but YNAB can create the placeholder again, and the model put it
first 4 times in the M3-dryrun window.

**Descriptions in `policy.json`** (option A):
- Remove `Dining Out`, `Shopping` and `Household & Kids`. Rename `Groceries` to `Groceries & Household`: *"Grocery
  stores, supermarkets and warehouse clubs, and household supplies and kids' everyday needs from any store.
  Restaurants and takeout are Guilt-Free Spending."*
- Add `Guilt-Free Spending`: *"Wants: restaurants, fast food, takeout, coffee and treats; Amazon orders and general
  shopping; hobbies, entertainment, and gifts not paid from a birthday or holiday fund."*
- Add `Home - Kids Classes & Education`: *"Lessons, classes, tutoring and school fees, voice lessons included."*
- Add `Subs - Amazon Prime`: *"Only the Amazon Prime membership fee. Amazon orders are Guilt-Free Spending."*
- Without these edits, `Candidates` warns about descriptions for categories that no longer exist.

## Scope 6: the spec

`deploy/restructure/csp-2026-11b.json`, in the `csp-2026-11.json` format: every visible category listed by its target
group. The dry run on the live plan (`./finbot restructure --spec deploy/restructure/csp-2026-11b.json --plan <active
plan>`, no `--apply`; `--used` is needed only with `--apply`, so this was one GET):

```
  1. rename "Groceries" to "Groceries & Household" (stays in "Fixed Costs")
  2. set target of "Home - Kids Classes & Education" to $810.00
  3. rename "Fun - Larks Dues" to "Larks Dues" and move it from "Guilt-Free" to "Fixed Costs"
  4. set target of "Larks Dues" to $25.00 by 2027-02-18
  5. move "Fun - Next Trip" from "Guilt-Free" to "Savings Goals"
  6. rename "Business Bike " to "Business Bike" and move it from "Guilt-Free" to "Savings Goals"
  7. rename "Shopping" to "Guilt-Free Spending" (stays in "Guilt-Free")
  8. move "Dining Out" from "Guilt-Free" to "Retired"
  9. move "Household & Kids" from "Fixed Costs" to "Retired"
 10. move "Voice Lessons" from "Guilt-Free" to "Retired"
Not in the spec (left where they are), 9: all already hidden
unresolved: 0
warnings: 0
changes: 10
```

Notes:
- **Merges are "hide the old category".** The API can't hide anything, so the spec moves each merged category into
  Retired, and Brendan hides the group in YNAB.
- **The spec can't move past transactions.** Dining Out, Household & Kids and Voice Lessons keep their transactions
  and their Available balances. Recategorizing them, if wanted, is by hand in YNAB.
- The Kids Classes target of $810 is the old $400 plus Voice Lessons' $410, both monthly. Change the number in the spec
  if that's wrong.
- **Larks Dues' target date:** its quarterly target says 2026-11-18, which has passed. Without an explicit date the
  tool would roll it forward a whole year, so the spec sets 2027-02-18, one quarter on. Check it in YNAB.

## Order of operations for Brendan

1. **Decide:** option A (the spec as-is) or B (the edit under "Groceries vs Household & Kids"), and the names
   `Guilt-Free Spending` and `Groceries & Household`. Review the 10 printed changes.
2. **Build the follow-ups, still in `dry_run`:** the alias map in calibration, plus the policy changes (the `Radar`
   keyword, `Needs a job!`, descriptions). Optionally, dry-run answers as labels. The policy can't deploy before the
   restructure, since its descriptions name categories that don't exist yet. Have it ready and deploy it right after
   step 4.
3. **Restructure**, with Brendan's OK for the exact list: `finbot restructure --spec
   deploy/restructure/csp-2026-11b.json --apply --confirm-live --used <n>`.
4. **In YNAB by hand:** move the Available balances of Dining Out → Guilt-Free Spending, Voice Lessons → Kids Classes,
   and Household & Kids → Groceries & Household. Recategorize this month's transactions in them if wanted. Then hide
   Retired, plus the empty Subscriptions, Monthly Variable Expenses and Target Balance Goals groups (from M3-dryrun).
   Check Larks Dues' target date and reorder categories. Then deploy the policy from step 2.
5. **Populate `plan_scan`** wherever calibration runs (`finbot sync --archived`), then **recalibrate** (BA-20,
   Brendan's) with the alias map in place.
6. **Dry-run under the new hashes** until the would-apply precision has a Wilson lower bound of 85%, or until Brendan
   decides the evidence is enough. At about 4 judged would-apply answers a day, that's weeks, not days.
