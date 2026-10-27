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
