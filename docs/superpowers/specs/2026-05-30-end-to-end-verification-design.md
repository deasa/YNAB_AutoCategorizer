# End-to-End Verification Design

**Date:** 2026-05-30
**Goal:** Prove the YNAB AutoCategorizer works end-to-end against real services
(YNAB + Turso/libSQL + Vertex AI), producing correct, safe categorizations on
real transactions.

## Background

The codebase is functionally complete: build, tests, and `go vet` are green, and
the full pipeline is wired (fetch uncategorized txns → AI picks a category →
embed + vector-search to resolve to a real category → safety checks → update
YNAB). A large uncommitted diff implements most of `plans/round1review.md` and
removes a now-unwanted "learning from historical transactions" feature
(`cmd/learn` deleted, `drop-learned-payees` migration added).

"Wired up" is not "verified." Nothing in this state has been proven against the
user's real YNAB budget, Turso DB, and Vertex AI. This effort closes that gap.

## Current Live Environment (verified during brainstorming)

- **Credentials ready:** YNAB token + budget, Turso URL + auth token, GCP project
  with Vertex AI; `gcloud` Application Default Credentials work. `.env` is
  populated for the Vertex path (no OpenAI key).
- **AI provider:** Vertex AI — `text-embedding-004` (768-dim, matches the
  `F32_BLOB(768)` schema) for embeddings, `gemini-2.0-flash` for categorization.
- **Turso DB state (inspected read-only):**
  - `searchable_categories`: **0 rows** — never successfully seeded. No stale or
    mixed-provider vectors to worry about; seeding is a clean slate.
  - `learned_payees`: **480 rows**, table still present (the historical-learning
    feature ran before being removed in code).
  - Migration tracking is split: the current runner's `schema_migrations`
    (tracked by `filename`) is **empty**; a legacy `migrations` table records
    only `0001-init`. The runner was rewritten and has never run against this DB.

## Key Findings That Shape the Plan

1. **Migration chain is self-healing.** Because `schema_migrations` is empty, the
   next `migrate.Run()` reattempts 0001–0006. Traced against current DB state it
   lands cleanly on the desired end state:
   - 0001 — `CREATE … IF NOT EXISTS` → no-ops.
   - 0002 — drops & recreates the empty `searchable_categories` (zero data loss),
     rebuilds the vector index.
   - 0003 — `CREATE TABLE IF NOT EXISTS learned_payees` → no-op.
   - 0004 — rebuilds via `learned_payees_v2`, copies 480 rows, renames over.
   - 0005 — re-adds `count` column.
   - 0006 — `DROP TABLE IF EXISTS learned_payees` → gone.
   - Result: empty `searchable_categories`, `learned_payees` dropped,
     `schema_migrations` populated 0001–0006.

2. **`cmd/seed` is non-idempotent.** `SaveEmbeddings` (`datastore/mapper.go:29`)
   is a plain `INSERT`; re-seeding duplicates every category. Moot for this run
   (table starts empty) but a real hygiene bug — fixed as part of the work.

3. **No transaction limit.** The app processes *all* uncategorized transactions
   in one pass. For a cautious first live run we add a max-N limit.

## Plan

### Phase 1 — Migrate + clean seed
- Run migrations (via `cmd/seed` startup). Verify: `learned_payees` dropped,
  `schema_migrations` lists 0001–0006, `searchable_categories` empty, vector
  index intact.
- Make `cmd/seed` idempotent: delete all rows in `searchable_categories` before
  inserting (clear-then-seed). Add a `DeleteAllEmbeddings` (or equivalent) to the
  `SearchStore` interface + `Mapper`, with a unit test.
- Run `go run ./cmd/seed`. Verify row count == number of active (non-hidden,
  non-deleted) YNAB categories, with zero duplicates.

### Phase 2 — Dry-run categorization (no YNAB writes)
- `DRY_RUN=true`, run once. Read logs for real uncategorized transactions:
  AI suggestion → vector match → distance → flag decisions →
  "would categorize → X".
- Sanity-check decisions and whether the default thresholds (max vector distance
  0.8, AI certainty 0.75, top-2 ambiguity gap) behave well on real data. Tune
  defaults only if clearly miscalibrated.

### Phase 3 — Limited live run
- Add a `MAX_TRANSACTIONS` config (env var; 0 = unlimited) honored by
  `Categorizer.Run()` so only the first N uncategorized transactions are
  processed. Cover with a unit test.
- `DRY_RUN=false`, small N (e.g., 3–5). Run once. Verify in the YNAB app that
  those transactions were categorized and that low-confidence / ambiguous ones
  are flagged orange.
- Optionally raise/remove the limit for a full pass once satisfied.

### Phase 4 — Lock it in
- Remove the throwaway `cmd/inspect` diagnostic.
- Drop the now-orphaned legacy `migrations` table (cleanup; optional, low risk).
- Commit the validated WIP diff plus the seed-idempotency and `MAX_TRANSACTIONS`
  changes as a coherent change. Update `.env.example` / `CLAUDE.md` for the new
  `MAX_TRANSACTIONS` setting.

## Out of Scope (explicitly deferred)
- Parallelizing AI calls (review #9) and early category validation (#7).
- Deployment / scheduling to a host (this session ends at a verified local run).
- Re-introducing any learning-from-history feature.

## Success Criteria
- `cmd/seed` populates `searchable_categories` with exactly the active YNAB
  categories (no dupes) using Vertex embeddings.
- A dry run produces sensible category decisions with correct flag behavior.
- A limited live run categorizes real transactions in YNAB, verified in-app,
  with appropriate orange flags.
- The WIP changes are committed; `go build`, `go test ./...`, `go vet ./...` all
  pass.
