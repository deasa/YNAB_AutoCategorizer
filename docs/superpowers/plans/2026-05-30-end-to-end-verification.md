# End-to-End Verification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make two small code changes (idempotent seeding, a `MAX_TRANSACTIONS` limit) and then verify the YNAB AutoCategorizer end-to-end against real YNAB + Turso + Vertex AI, ending with the validated work committed.

**Architecture:** Code tasks (1–3) are TDD against the existing mock/sqlmock test suites. Verification tasks (4–7) are operational: run the real binaries against live services with `DRY_RUN` and a transaction limit as safety gates, observing output before each escalation. Live-service tasks must run in the main session (they need the user's `.env` and `gcloud` ADC); they cannot be delegated to an isolated subagent.

**Tech Stack:** Go 1.25, libSQL/Turso (`vector_top_k`, `F32_BLOB(768)`), Vertex AI (`text-embedding-004`, `gemini-2.0-flash`), `go-sqlmock` for datastore tests, hand-rolled mocks for categorizer/config tests.

**Spec:** `docs/superpowers/specs/2026-05-30-end-to-end-verification-design.md`

**Branch:** `verify-end-to-end` (already created; spec already committed).

---

## File Structure

- `datastore/mapper.go` — add `DeleteAllEmbeddings()` to `*Mapper` (clears `searchable_categories`).
- `datastore/mapper_test.go` — sqlmock tests for the new method.
- `cmd/seed/main.go` — clear the table before seeding (idempotent re-seed).
- `config/config.go` — add `MaxTransactions int`, parsed from `MAX_TRANSACTIONS` (default 0 = unlimited).
- `config/config_test.go` — parsing test.
- `categorizer/categorizer.go` — add `maxTransactions` field + `SetMaxTransactions(int)`; honor it in `Run()`.
- `categorizer/categorizer_test.go` — limit-truncation test.
- `main.go` — wire `cfg.MaxTransactions` into the categorizer.
- `.env.example`, `CLAUDE.md` — document `MAX_TRANSACTIONS`.
- `cmd/inspect/` — deleted in Task 7 (throwaway diagnostic).

---

## Task 1: Idempotent seed — `DeleteAllEmbeddings` on the Mapper

**Files:**
- Modify: `datastore/mapper.go`
- Test: `datastore/mapper_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `datastore/mapper_test.go`:

```go
func TestDeleteAllEmbeddings_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("DELETE FROM searchable_categories").
		WillReturnResult(sqlmock.NewResult(0, 42))

	m := NewMapper(db)
	if err := m.DeleteAllEmbeddings(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDeleteAllEmbeddings_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("DELETE FROM searchable_categories").
		WillReturnError(fmt.Errorf("db delete error"))

	m := NewMapper(db)
	if err := m.DeleteAllEmbeddings(); err == nil {
		t.Fatal("expected error, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./datastore -run TestDeleteAllEmbeddings -v`
Expected: FAIL — compile error `m.DeleteAllEmbeddings undefined`.

- [ ] **Step 3: Implement `DeleteAllEmbeddings`**

Add to `datastore/mapper.go` (after `SaveEmbeddings`):

```go
// DeleteAllEmbeddings removes every row from searchable_categories.
// Used by the seeder to start from a clean slate so re-seeding does not
// create duplicate category rows.
func (m *Mapper) DeleteAllEmbeddings() error {
	if _, err := m.db.Exec(`DELETE FROM searchable_categories`); err != nil {
		return fmt.Errorf("error deleting embeddings: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./datastore -run TestDeleteAllEmbeddings -v`
Expected: PASS (both cases).

- [ ] **Step 5: Commit**

```bash
git add datastore/mapper.go datastore/mapper_test.go
git commit -m "feat(datastore): add DeleteAllEmbeddings for idempotent seeding

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 2: Clear the table before seeding

**Files:**
- Modify: `cmd/seed/main.go`

Note: `cmd/seed` has no test (it's a thin live-services entrypoint); the logic it relies on is covered by Task 1. This task is a wiring change verified at runtime in Task 5.

- [ ] **Step 1: Clear before the seed loop**

In `cmd/seed/main.go`, immediately after `mapper := datastore.NewMapper(db)` and before constructing `searchService`, add:

```go
	// Start from a clean slate so re-seeding does not duplicate categories.
	if err := mapper.DeleteAllEmbeddings(); err != nil {
		logger.Fatalf("error clearing existing categories: %v", err)
	}
	logger.Println("cleared existing categories from vector database")
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./...`
Expected: exit 0, no output.

- [ ] **Step 3: Commit**

```bash
git add cmd/seed/main.go
git commit -m "feat(seed): clear searchable_categories before seeding

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 3: `MAX_TRANSACTIONS` limit

**Files:**
- Modify: `config/config.go`, `categorizer/categorizer.go`, `main.go`
- Test: `config/config_test.go`, `categorizer/categorizer_test.go`

- [ ] **Step 1: Write the failing config test**

Add to `config/config_test.go`:

```go
func TestLoad_MaxTransactions(t *testing.T) {
	t.Setenv("YNAB_ACCESS_TOKEN", "tok")
	t.Setenv("LIBSQL_DATABASE_URL", "libsql://example")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("MAX_TRANSACTIONS", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxTransactions != 5 {
		t.Errorf("MaxTransactions = %d, want 5", cfg.MaxTransactions)
	}
}

func TestLoad_MaxTransactions_DefaultsZero(t *testing.T) {
	t.Setenv("YNAB_ACCESS_TOKEN", "tok")
	t.Setenv("LIBSQL_DATABASE_URL", "libsql://example")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxTransactions != 0 {
		t.Errorf("MaxTransactions = %d, want 0", cfg.MaxTransactions)
	}
}
```

Note: if existing config tests already set a different AI key combination, mirror their setup. The required fields are `YNAB_ACCESS_TOKEN`, `LIBSQL_DATABASE_URL`, and one AI provider key.

- [ ] **Step 2: Run config test to verify it fails**

Run: `go test ./config -run TestLoad_MaxTransactions -v`
Expected: FAIL — `cfg.MaxTransactions undefined`.

- [ ] **Step 3: Implement config parsing**

In `config/config.go`, add to the `Config` struct (under `// App settings`):

```go
	MaxTransactions     int // max uncategorized txns to process per run; 0 = unlimited
```

In `Load()`, after the run-interval block and before `// Validate required fields`, add:

```go
	// Max transactions per run (0 = unlimited)
	maxTxnStr := getEnvDefault("MAX_TRANSACTIONS", "0")
	maxTxn, err := strconv.Atoi(maxTxnStr)
	if err != nil {
		return nil, fmt.Errorf("invalid MAX_TRANSACTIONS %q: %w", maxTxnStr, err)
	}
	cfg.MaxTransactions = maxTxn
```

- [ ] **Step 4: Run config test to verify it passes**

Run: `go test ./config -run TestLoad_MaxTransactions -v`
Expected: PASS (both cases).

- [ ] **Step 5: Write the failing categorizer test**

Add to `categorizer/categorizer_test.go`:

```go
func TestRun_MaxTransactions_LimitsProcessing(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
			makeTxn("txn-2", strPtr("Whole Foods"), nil),
			makeTxn("txn-3", strPtr("Whole Foods"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.02},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	cat.SetMaxTransactions(2)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 2 {
		t.Fatalf("expected 2 updates (limit), got %d", len(ynabClient.updatedTxns))
	}
}
```

- [ ] **Step 6: Run categorizer test to verify it fails**

Run: `go test ./categorizer -run TestRun_MaxTransactions_LimitsProcessing -v`
Expected: FAIL — `cat.SetMaxTransactions undefined`.

- [ ] **Step 7: Implement the limit in the categorizer**

In `categorizer/categorizer.go`, add a field to the `Categorizer` struct (after `dryRun`):

```go
	maxTransactions     int // 0 = unlimited
```

Add a setter after `New`:

```go
// SetMaxTransactions caps how many uncategorized transactions a single Run
// will process. 0 (the default) means unlimited.
func (c *Categorizer) SetMaxTransactions(n int) {
	c.maxTransactions = n
}
```

In `Run()`, immediately after the `if len(txns) == 0 { ... }` block, add:

```go
	// Apply the per-run transaction limit (0 = unlimited).
	if c.maxTransactions > 0 && len(txns) > c.maxTransactions {
		c.logger.Printf("limiting run to first %d of %d uncategorized transactions", c.maxTransactions, len(txns))
		txns = txns[:c.maxTransactions]
	}
```

- [ ] **Step 8: Run categorizer test to verify it passes**

Run: `go test ./categorizer -run TestRun_MaxTransactions_LimitsProcessing -v`
Expected: PASS.

- [ ] **Step 9: Wire config into main.go**

In `main.go`, replace the categorizer construction line:

```go
	cat := categorizer.New(ynab, aiProvider, searchService, logger, cfg.ConfidenceThreshold, cfg.DryRun)
```

with:

```go
	cat := categorizer.New(ynab, aiProvider, searchService, logger, cfg.ConfidenceThreshold, cfg.DryRun)
	cat.SetMaxTransactions(cfg.MaxTransactions)
```

- [ ] **Step 10: Document the new setting**

In `.env.example`, add a line near the other app settings:

```
# Max uncategorized transactions to process per run (0 = unlimited)
MAX_TRANSACTIONS=0
```

In `CLAUDE.md`, under the Configuration section, add a sentence:

```
`MAX_TRANSACTIONS` caps how many uncategorized transactions are processed per run (0 = unlimited); useful for a cautious first live run.
```

- [ ] **Step 11: Full build + test + vet**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: all pass, exit 0.

- [ ] **Step 12: Commit**

```bash
git add config/config.go config/config_test.go categorizer/categorizer.go categorizer/categorizer_test.go main.go .env.example CLAUDE.md
git commit -m "feat: add MAX_TRANSACTIONS limit for cautious live runs

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

---

## Task 4: Phase 1 — run migrations against the real DB

**Files:** none (operational; runs in main session with live `.env` + `gcloud` ADC).

Pre-req: `gcloud auth application-default print-access-token` succeeds and `.env` is populated for the Vertex path.

- [ ] **Step 1: Trigger migrations via a no-op seed dry of the migrator**

Migrations run on `cmd/seed` startup. To run migrations and seeding together, proceed to Task 5 — but first confirm the migration outcome in isolation by running the inspector before and after.

Run (before): `go run ./cmd/inspect`
Record: `searchable_categories` row count (expect 0), `learned_payees` present (expect 480 rows), `schema_migrations (filename)` empty.

- [ ] **Step 2: Run migrations by running the seeder (also performs Task 5)**

This step is executed as part of Task 5. After Task 5 completes, run the inspector again.

Run (after): `go run ./cmd/inspect`
Expected:
- `schema_migrations (filename)` now lists all six files `2026-03-06-0001-init.sql` … `2026-03-30-0006-drop-learned-payees.sql`.
- `learned_payees`: "table gone or error" (dropped by 0006).
- `searchable_categories`: row count == number of active YNAB categories (verified in Task 5).

If migrations error out (e.g., a non-idempotent statement fails), STOP and report the exact error before proceeding — do not hand-edit the DB.

---

## Task 5: Phase 1 — clean seed with Vertex embeddings

**Files:** none (operational).

- [ ] **Step 1: Run the seeder**

Run: `go run ./cmd/seed`
Expected output: "cleared existing categories…", one "seeding category: …" line per active category, then "done: seeded N categories into vector database".

- [ ] **Step 2: Verify row count and no duplicates**

Run: `go run ./cmd/inspect`
Expected: `searchable_categories` row count == N from the seed log, "distinct categories with duplicates: 0".

- [ ] **Step 3: Cross-check N against YNAB**

Confirm N equals the number of active (non-hidden, non-deleted) categories in the YNAB budget. If it is far off, STOP and investigate (hidden/deleted filtering or a YNAB API issue) before continuing.

---

## Task 6: Phase 2 — dry-run categorization

**Files:** none (operational).

- [ ] **Step 1: Run in dry-run mode**

Run: `DRY_RUN=true go run .`
The app does one immediate pass then starts a ticker — let the first pass finish, then Ctrl-C.

- [ ] **Step 2: Read the decisions**

For each uncategorized transaction the log shows: AI suggestion, vector best match + distance, any orange-flag reasons, and a `[DRY RUN] would categorize … → <category>` line.

Verify:
- Categories chosen are sensible for the payees.
- Distances for good matches are comfortably under 0.8; genuine non-matches are skipped.
- Low-certainty (<0.75) and ambiguous (top-2 gap small) cases are flagged.

- [ ] **Step 3: Tune only if clearly miscalibrated**

If thresholds are obviously wrong on real data, adjust defaults (`defaultMaxVectorDistance` in `categorizer/categorizer.go`, or `CONFIDENCE_THRESHOLD` in `.env`) and re-run Step 1. Otherwise leave defaults. Record observations in the verification summary; do not commit threshold changes unless made.

---

## Task 7: Phase 3 + 4 — limited live run, cleanup, lock it in

**Files:** delete `cmd/inspect/`; final commit of the WIP diff.

- [ ] **Step 1: Limited live run**

Run: `DRY_RUN=false MAX_TRANSACTIONS=3 go run .`
Let the first pass complete, then Ctrl-C.

- [ ] **Step 2: Verify in the YNAB app**

Open YNAB and confirm those (≤3) transactions are now categorized, and that any flagged-orange ones in the log show an orange flag in-app. If anything looks wrong, STOP and report before a wider run.

- [ ] **Step 3: Optional wider live run**

If satisfied, run a full pass: `DRY_RUN=false go run .` (let it finish, Ctrl-C). Skip this step if the user prefers to stop at the sample.

- [ ] **Step 4: Remove the throwaway inspector**

```bash
git rm -r cmd/inspect
go build ./...
```
Expected: build exits 0.

- [ ] **Step 5: Final verification before committing the WIP**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: all pass.

- [ ] **Step 6: Commit the validated work**

```bash
git add -A
git commit -m "feat: verify end-to-end categorization; remove learning feature and inspector

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
```

- [ ] **Step 7: Report verification summary**

Summarize: categories seeded (N), dry-run decision quality, live-run result confirmed in YNAB, any threshold tuning, and final build/test/vet status. Then invoke `superpowers:finishing-a-development-branch` to decide on merge/PR.

---

## Notes / Deferred
- Legacy `migrations` table is left in place (harmless cruft; dropping it is optional and out of scope).
- Parallelizing AI calls (review #9) and early category validation (#7) remain deferred.
- Live-service tasks (4–7) must run in the main session; do not delegate to an isolated subagent without the user's `.env` and ADC.
