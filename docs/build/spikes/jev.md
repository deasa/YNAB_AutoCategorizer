# Spike M0-1: Jev as finbot's transaction classifier (U1)

Probe: `spikes/jev/` (its own module, stdlib only), run 2026-10-27 against TypeSafe `jev-1.14.0` and Brendan's live YNAB
plan, read-only. Run it with `cd spikes/jev && go run . -cache <dir outside repo> [-variants …] [-offset 100]`. Fetched
data and Jev answers are cached in `-cache`, so a rerun re-scores without spending calls. Nothing captured from either API
is committed. Only the aggregates below are.

Labels: **VERIFIED** means observed in this probe's live calls. **UNVERIFIED** means taken from docs or inferred.
**Sample size warning:** each table covers 100 transactions (200 for the best variant across two disjoint samples).
Treat every percentage as ±~8 points. The numbers are indicative only, and `finbot calibrate` must re-measure τ.

## Setup

| Item | Value |
|---|---|
| Candidates | **47** categories: non-hidden, non-deleted, minus 6 Birthday/Gift, 13 Credit Card Payments, 3 Internal Master (Ready to Assign). Hidden: 11 excluded |
| Labeled pool | approved + categorized + not transfer + not split: 416 transactions in the window, of which 394 have a candidate category (22 are in hidden or excluded categories) |
| Window | **2025-10-20 .. 2026-04-20**: the 6 months ending at the latest approved transaction. Nothing has been approved since April 2026, so "last 6 months from today" holds 0 labeled transactions. Unapproved categorized transactions were not used as labels (the old bot could have set them) |
| Sample | seeded shuffle of the pool. Sample A = pool[0:100] (exploration). Sample B = pool[100:200] (**holdout**, used only to validate the variant chosen on A) |
| Payee history | ~55% of sampled transactions have ≥1 earlier labeled transaction from the same payee (last 5 labels used) |
| Calls | Jev: **300** (cap 300; 47 of them wasted, see Result). YNAB: **3 GETs** (`/categories`, `/transactions` ×2) |

## API shape (VERIFIED)

- `POST https://api.typesafe.ai/v1/systemone`, `Authorization: Bearer <key>`, JSON. Request:
  `{model, state, questions: {<id>: {type:"choice", instructions, criteria: {<option name>: <description>}}}}`.
  `state` is a string, an object or an array. Response: `{model, answers: {<id>: {type, choice, confidence,
  probabilities: {<option>: p}}}, usage: {input_tokens, output_tokens}}`. `model` echoes the versioned id (`jev-1.14.0`).
- Question type: **Choice**. Options are the category names. Each option's description is `Group: <group>. Note: <note>`.
  State per transaction: `payee`, `amount` (`$12.34 outflow (money spent)`), `date` (with weekday), `account`, `memo`
  (omitted if empty), `payee_previous_categories` (omitted if none). Instructions: "Which budget category does this
  transaction belong to?"
- `confidence` behaves almost the same as the chosen option's probability (`p_top`). The curves differ by ≤1 transaction
  at any τ, so either works as the gate.
- Batching: **yes**. One request holds many questions over shared state. Batch form: state `{transactions: {t01: {...},
  ...}}`, one Choice per transaction referring to `` `transactions.t01` ``. Each question repeats the full criteria (~1.4k
  tokens), so batching saves little in tokens (~12%). Its benefit is fewer round-trips.
- Limits (UNVERIFIED, from the models page): 64k tokens per request and 32k per question. A Choice allows ≤255 options.
  Rate limits are per organization, in tokens/s and requests/s, shown in the console (not checked). Exceeding them returns
  429 with `retry-after`. At ~1.5k tokens per transaction a batch fits ~40 transactions under 64k (UNVERIFIED, inferred).
  We tested batches of 10.

## Results

Variants: **single** (1 transaction per request, no history). **batch10** (10 per request). **+history** adds the payee's
last 5 labels to state (§3.2 step 3). **+policy** gives `Discretionary` the household rule the current app's prompt already
uses (CLAUDE.md): "Everyday spending: dining out, restaurants, groceries, Amazon orders and general shopping." That is the
category's only description change. All other categories keep their YNAB note.

### Accuracy and coverage at τ (p_top gate)

| Variant | top-1 | top-3 | cov@τ0.7 / prec | cov@τ0.8 / prec | cov@τ0.95 / prec | lowest τ for ≥85% prec → coverage |
|---|---|---|---|---|---|---|
| single (A) | 58% | 76% | 37% / 46% | 32% / 50% | 21% / 62% | never reaches 85% |
| batch10 (A) | 48% | 72% | 37% / 41% | 30% / 43% | 20% / 65% | never reaches 85% |
| single+history (A) | 67% | 88% | 54% / 85% | 50% / 88% | 43% / 88% | 0.72 → 54% |
| batch10+history (A) | 69% | 93% | 72% / 81% | 62% / 84% | 44% / 91% | 0.74 → 67% |
| batch10+history (B, holdout) | 72% | 91% | 68% / 85% | 62% / 89% | 46% / 94% | 0.72 → 68% |
| **batch10+history+policy (A)** | **78%** | 93% | 79% / 85% | 73% / 89% | 56% / 96% | 0.67 → 82% |
| **batch10+history+policy (B, holdout)** | **80%** | 91% | 78% / 90% | 73% / 90% | 55% / 91% | 0.46 → 91% |

All VERIFIED (measured). Pooled A+B for the recommended variant: **τ = 0.8 → 73% coverage at ~90% precision (131/146)**.

### Rules layer vs Jev vs hybrid (§3.2 step 2 + 3)

| Approach | Sample A cov / prec | Sample B cov / prec |
|---|---|---|
| Rules only: payee's majority previous label | 55% / 84% | 56% / 89% |
| Rules only: unanimous payee history | 47% / 94% | 54% / 91% |
| **Hybrid: unanimous rule, else Jev (+history+policy) at τ 0.8** | **76% / 91%** | **74% / 92%** |
| Hybrid at τ 0.9 | 72% / 93% | 69% / 93% |

Jev adds ~20 points of coverage beyond the rules layer at the same or better precision (VERIFIED). On transactions whose
payee has no history, the policy variant still gets 70–71% top-1 (vs 57–60% without policy).

### Calibration (p_top bucket → observed accuracy)

| Bucket | single (A) | batch10+history (A) | +policy (A) | +policy (B, holdout) |
|---|---|---|---|---|
| [0.00, 0.50) | 42 → 69% | 13 → 23% | 9 → 44% | 11 → 27% |
| [0.50, 0.70) | 21 → 57% | 15 → 53% | 12 → 58% | 11 → 64% |
| [0.70, 0.80) | 5 → 20% | 10 → 60% | 6 → 33% | 5 → 80% |
| [0.80, 0.90) | 9 → 22% | 12 → 67% | 8 → 50% | 12 → 92% |
| [0.90, 0.95) | 2 → 50% | 6 → 67% | 9 → 78% | 6 → 83% |
| [0.95, 1.00] | 21 → 62% | 44 → 91% | 56 → 96% | 55 → 91% |

(cells: n → observed accuracy)

- **Without history or policy, the probabilities are not usable** (VERIFIED). High p_top was wrong more often than low.
  Of the misses, 65% had the human label `Discretionary`. The household files hardware stores, bike shops, Amazon and
  similar under the catch-all, and Jev confidently picks the semantically "right" category instead. Examples: Ace Hardware →
  Home Improvement (p 0.84), Amazon → Subs - Amazon Prime (p 0.87), Bike Peddler → Business Bike. The model is calibrated
  for "what category is this purchase", not "where does *this household* file it". Calibration depends on the state
  carrying the household's conventions.
- With history and policy, the probabilities rank well (monotone, more or less), but the **top bucket is overconfident**:
  p ≈ 0.99 → 91–96% observed. Raising τ can't take precision much above ~92–95%. This confirms §3.3: never trust raw
  confidence, and take τ from the measured curve.

### Latency (VERIFIED, wall time from this Mac, per request)

| Variant | requests | p50 | p95 | max |
|---|---|---|---|---|
| single | 100 | 0.11 s | 0.18 s | 0.26 s |
| single+history | 100 | 0.11 s | 0.18 s | 0.31 s |
| batch of 10 (5 runs × 10 req) | 50 | 0.16–0.17 s | 0.20–0.29 s (one run 1.08 s) | 1.08 s (one outlier) |

A batch of 10 takes about 1.5× one single request, so ~17 ms per transaction. Neither latency matters for a 15-minute
sync.

### Cost

- Price: **$0.042 per million input tokens**, and output tokens are free (models page: UNVERIFIED against an invoice).
- Tokens: VERIFIED from `usage.input_tokens`. That's 1,696–1,714 per transaction single and 1,479–1,513 per transaction
  batched (47 candidates; the criteria dominate).
- **≈ $0.06–0.07 per 1,000 transactions** (computed from both: UNVERIFIED). At this plan's ~150 transactions a month that
  is about $0.01 a month. Cost is a non-issue. This whole spike (300 calls, ~1.2M tokens) cost about $0.05.

## Recommendation

**Jev: primary classifier**, with conditions (VERIFIED on 200 samples; τ indicative only):

1. Run it **only with the rules layer and payee history in state**, plus a written household policy for the catch-all
   category, set as its criterion. Bare Jev (58% top-1, unusable calibration) would be **rejected**.
2. **Starting τ = 0.8 on p_top** (pooled ~90% precision, ~73% coverage). It meets R6's 85% with margin, given the ±8 point
   noise. Order: unanimous payee-history rule → Jev ≥ 0.8 → Slack top-3 question. The top-3 contains the right answer
   91–93% of the time, so the Slack buttons will usually hold it.
3. `finbot calibrate` must re-derive τ on a larger labeled set (the whole archived history, hundreds of transactions)
   before the routine tier auto-approves. Also, our policy text was chosen after seeing sample A's misses. It is the
   existing app's rule, not fitted wording, and holdout B confirms it. Even so, keep the policy in config and re-measure
   it whenever it changes.
4. Batch 10 (or more) transactions per request. Accuracy matched or beat single-request accuracy when history was in
   state (69% vs 67%). Without history, batching scored 10 points lower (58% vs 48%), which may be noise or batch
   cross-talk. Re-check in calibrate.

The headless `claude` fallback is still useful as a second opinion on the τ-miss band. Not tested here.

## Impact on architecture (§3.3 `classify.Model`)

```go
type Txn struct {
    ID, Payee, Memo, Account string
    Date                     time.Time
    AmountMilli              int64
    PayeeHistory             []string // last N human labels for this payee, newest first; nil if none
}

type Candidate struct{ Name, Group, Description string } // Description = YNAB note, or the policy override

type Prediction struct {
    Probs map[string]float64 // per candidate name; sums ≈ 1
    Top   []string           // ordered by probability (top-3 → Slack buttons)
    Model string             // versioned id the provider reported, logged in `decisions`
}

type Model interface {
    // Classify is batch-shaped: Jev answers many txns per request, and the claude CLI wants batches too.
    Classify(ctx context.Context, txns []Txn, cands []Candidate) ([]Prediction, error)
}
```

- **Batch-first interface.** Both candidates want batches, so a single-transaction call would be the special case.
- **Payee history is input, not just a rule.** The rules layer (§3.2 step 2) still decides unanimous cases. It must also
  pass history to the model, because history is the biggest single accuracy gain (58% → 67–69%).
- **Candidate descriptions need an override.** The catch-all's YNAB note is empty, and the household convention has to live
  somewhere. Put it in a config map `category → description`, versioned with calibration.
- **Pin the model id** (`jev-1.14.0`, not `jev-latest`) and store `Prediction.Model` in `decisions`. τ is only valid for
  the version it was calibrated on.
- **A failed call is an error, not "uncertain".** Leave the transaction for the next sync and never map the failure to a
  Slack question or a default.
- **Gate on `p_top`.** Jev's `confidence` adds nothing over `p_top`, so the interface can keep just the probabilities.
- 47 candidates × ~30 tokens is most of the request. If the category list grows past ~200, look at hierarchical Choice
  (group first) from the TypeSafe cookbook. Not needed now.

## Ideas for later (not tested: prompt-tuning cap)

- A companion Noul, "Is this a routine purchase this household categorizes automatically?", to replace the `NONE`
  sentinel in the current app.
- Write descriptions for more categories (13 of 47 have notes). The `Holiday - Christmas` and `Home - Tech` misses look
  like convention gaps, like Discretionary.
- Try 3 instead of 5 history labels, and include each label's date.
- Try larger batches (≈40) to measure cross-talk vs batch size.

## Per-category difficulty (for the category simplification)

Task M0-1b, computed **offline** from M0-1's cached answers with zero API calls. The Jev call counter stayed at 300. To
reproduce: `cd spikes/jev && go run . -cache <dir> -percat`. `-percat` implies `-offline`, which fails rather than call
YNAB or Jev. Scope: `batch10+history+policy`, samples A and B pooled (n=200, top-1 79%). The last column is the bare
`single` variant on sample A (n=100), for contrast. "<τ0.8" is the share of that category's transactions whose p_top is
below 0.8, so they would be left for review. Sorted hardest first (lowest top-1, then lowest mean p_top). **⚠ low-n** marks
n<3. A single transaction there decides 0% vs 100%, so treat those rows as anecdotes. All VERIFIED (measured), with
the sample-size caveat above.

| True category | n | top-1 | mean p_top | <τ0.8 | most common wrong pick | single top-1 (n) |
|---|---|---|---|---|---|---|
| Subs - Google Domains Website ⚠ low-n | 1 | 0% | 0.29 | 100% | Discretionary (1) | 0% (1) |
| Business Bike ⚠ low-n | 1 | 0% | 0.32 | 100% | Discretionary (1) | — |
| Fun - Brendan Under The Radar ⚠ low-n | 1 | 0% | 0.34 | 100% | Discretionary (1) | 0% (1) |
| Fun - Hilarie Under The Radar ⚠ low-n | 1 | 0% | 0.53 | 100% | Discretionary (1) | 0% (1) |
| Home - Tech / Computer Replacement | 5 | 0% | 0.76 | 80% | Discretionary (5) | 0% (3) |
| Talk About ⚠ low-n | 2 | 0% | 0.82 | 50% | Discretionary (1) | 0% (1) |
| Subs - GoPro ⚠ low-n | 1 | 0% | 0.97 | 0% | Subs - Disney+ (1) | 0% (1) |
| Holiday - Christmas | 5 | 20% | 0.90 | 20% | Discretionary (3) | 0% (3) |
| Fun - Next Trip | 4 | 25% | 0.47 | 100% | Auto - Repairs and Expenses (1) | 0% (1) |
| Giving - Generosity | 5 | 40% | 0.68 | 60% | Discretionary (3) | 0% (2) |
| Wise - Medical & Dental | 4 | 75% | 0.97 | 0% | Discretionary (1) | 100% (1) |
| Auto - Repairs and Expenses | 9 | 78% | 0.88 | 22% | Discretionary (2) | 100% (5) |
| Home - Kids Classes & Education | 6 | 83% | 0.81 | 33% | Voice Lessons (1) | 100% (2) |
| Discretionary | 126 | 87% | 0.86 | 25% | Reimbursement (4) | 61% (66) |
| Voice Lessons ⚠ low-n | 1 | 100% | 0.81 | 0% | — | 100% (1) |
| Home - Electric ⚠ low-n | 2 | 100% | 0.89 | 50% | — | — |
| Home - Improvement / Maintenance | 6 | 100% | 0.91 | 17% | — | 100% (3) |
| Home - Natural Gas ⚠ low-n | 2 | 100% | 0.96 | 0% | — | 0% (2) |
| Subs - Canva | 3 | 100% | 0.99 | 0% | — | 100% (1) |
| Subs - TrainerRoad ⚠ low-n | 2 | 100% | 0.99 | 0% | — | 100% (1) |
| Google One ⚠ low-n | 1 | 100% | 1.00 | 0% | — | — |
| Home - Highland City Utilities ⚠ low-n | 2 | 100% | 1.00 | 0% | — | — |
| Home - Internet ⚠ low-n | 1 | 100% | 1.00 | 0% | — | 100% (1) |
| Home - Mortgage ⚠ low-n | 2 | 100% | 1.00 | 0% | — | — |
| Stock Investments ⚠ low-n | 1 | 100% | 1.00 | 0% | — | — |
| Subs - Spotify ⚠ low-n | 2 | 100% | 1.00 | 0% | — | 100% (1) |
| Subs - YNAB | 4 | 100% | 1.00 | 0% | — | 100% (2) |

**Predicted categories with the lowest precision.** Only those below 100% are listed.

| Predicted | picked | precision | picked at p_top≥0.8 | precision at τ0.8 |
|---|---|---|---|---|
| Reimbursement | 4 | 0% | 1 | 0% |
| Subs - Disney+ | 3 | 0% | 3 | 0% |
| Subs - Amazon Prime | 2 | 0% | 1 | 0% |
| Subs - Bark | 1 | 0% | 1 | 0% |
| Subs - Lawn | 1 | 0% | 0 | — |
| Giving - Generosity | 4 | 50% | 3 | 33% |
| Fun - Next Trip | 2 | 50% | 0 | — |
| Holiday - Christmas | 2 | 50% | 1 | 100% |
| Voice Lessons | 2 | 50% | 1 | 100% |
| Home - Kids Classes & Education | 7 | 71% | 4 | 100% |
| Home - Improvement / Maintenance | 8 | 75% | 5 | 100% |
| Auto - Repairs and Expenses | 9 | 78% | 6 | 100% |
| Discretionary | 130 | 85% | 96 | 93% |

**Most frequent confusions (true → predicted).** "≥τ" counts the ones that would have been auto-applied wrongly.

| True → predicted | n | ≥τ0.8 |
|---|---|---|
| Home - Tech / Computer Replacement → Discretionary | 5 | 1 |
| Discretionary → Reimbursement | 4 | 1 |
| Holiday - Christmas → Discretionary | 3 | 3 |
| Giving - Generosity → Discretionary | 3 | 1 |
| Discretionary → Giving - Generosity | 2 | 2 |
| Discretionary → Subs - Disney+ | 2 | 2 |
| Auto - Repairs and Expenses → Discretionary | 2 | 1 |
| Discretionary → Subs - Amazon Prime | 2 | 1 |
| Discretionary → Home - Kids Classes & Education | 2 | 0 |

**Patterns:**
- **Discretionary is the gravity well.** It is the true label for 126 of 200 transactions, and 36 of the 42 errors involve
  it on one side or the other. For 10 of the 13 non-Discretionary categories that miss, Discretionary is the most common
  wrong pick.
- **Purpose-based categories can't be read off the transaction.** Tech / Computer Replacement, Christmas, Generosity, both
  Under The Radar categories, Next Trip, Business Bike and Google Domains name *why* money was spent, but the merchant is
  an ordinary store. Jev scores 0–40% on them. Either merge them into Discretionary, or keep them as manual-only categories
  (drop them from the candidate list, as Birthday/Gift already are).
- **Christmas is confidently wrong**, which τ can't catch: all 3 of its misses had p_top ≥ 0.8. It behaves like
  Birthday/Gift, so it's a candidate for `EXCLUDED_CATEGORY_KEYWORDS` (or a merge) rather than a description fix.
- **Subscriptions pull false positives.** Merchant-specific subscriptions with a history are near-perfect (Canva, YNAB,
  Spotify, TrainerRoad, Google One: 100%, p_top ≈ 1). But as predictions, Disney+, Amazon Prime, Bark and Lawn are
  0-for-7, and 5 of those 7 had p_top ≥ 0.8. At least 4 of the 7 were Discretionary purchases, and one was GoPro → Disney+,
  a sub-to-sub swap. Merging into one `Subscriptions` category would remove the sub-to-sub swaps. The Amazon Prime vs
  "Amazon orders → Discretionary" policy clash needs a description that says Prime is the membership fee only.
- **Reimbursement and Generosity need descriptions or removal.** Reimbursement is 0-for-4 as a prediction. Generosity is
  confused with Discretionary in both directions (3 + 2, three of them at ≥ τ). Neither has a note that tells Jev what
  qualifies.
- **Fixed bills are solved; keep them.** Mortgage, utilities, internet, electric and natural gas are 100% with p_top ≈ 1
  (low n each). The bare `single` variant gets Natural Gas 0/2, so payee history is what fixes them. Also, only 27 of the
  47 candidates appear as a true label at all in these 200 transactions. The other 20 are candidates for deletion, which
  also shrinks every request's criteria.
