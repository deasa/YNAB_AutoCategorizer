# Round 1 Code Review

## Bugs

### 1. `FindRelevantContent` — `sql.ErrNoRows` check is dead code
**File:** `datastore/mapper.go:51`

`db.Query()` never returns `sql.ErrNoRows` — that's only returned by `QueryRow().Scan()`. For `Query()`, an empty result set simply returns zero rows when iterating. This check will never trigger. It's harmless but misleading, and the test at `mapper_test.go:160` is testing behavior that can't happen in production (sqlmock explicitly returns the error, but the real driver won't).

**Fix:** Remove the `sql.ErrNoRows` check and the corresponding test.

### 2. `serializeEmbeddings` is fragile
**File:** `datastore/mapper.go:36-38`

```go
func serializeEmbeddings(embeddings []float32) string {
    return strings.Join(strings.Split(fmt.Sprintf("%v", embeddings), " "), ", ")
}
```

This relies on `fmt.Sprintf("%v")` formatting `[]float32` as `[0.1 0.2 0.3]` and then replacing spaces with `, `. The bigger issue is **precision loss** — `%v` for float32 uses the shortest representation, which may drop precision on embeddings. Should use explicit formatting per element for reliability.

**Fix:** Rewrite to iterate elements and format each with controlled precision (e.g., `strconv.FormatFloat` or `fmt.Sprintf("%g")`).

### 3. Value receiver on `search` methods
**File:** `search/search.go:50`, `search/search.go:63`

`search.Search()` and `search.InsertContent()` use value receivers. This works today since the struct only holds interface pointers, but it's unconventional and would break silently if mutable state were ever added. Pointer receivers would be more idiomatic.

**Fix:** Change to pointer receivers: `func (s *search) Search(...)` and `func (s *search) InsertContent(...)`.

### 4. `go.mod` specifies `go 1.25.7`
**File:** `go.mod:3`

Go version `1.25.7` doesn't exist. Go versioning is `1.2x` (e.g., 1.22, 1.23). This looks like a typo or placeholder.

**Fix:** Update to the actual Go version used to build this project.

## Correctness Concerns

### 5. No vector distance threshold — low-quality matches applied unconditionally
**File:** `categorizer/categorizer.go`

The CLAUDE.md documents: "Best match distance must be <= confidence threshold (default 0.7; lower distance = better match)". But this check **is no longer implemented** in the code. The `confidenceThreshold` is now compared against AI certainty only, not vector distance. Every transaction with a vector match gets categorized regardless of vector distance. A poor vector match (distance 0.99) still gets applied if the AI was confident.

**Fix:** Re-add a vector distance threshold check after the vector search, or update CLAUDE.md to reflect the new behavior. Consider whether the old safety check was intentionally removed or lost during the AI categorization refactor.

### 6. Category list sent to AI is non-deterministic
**File:** `categorizer/categorizer.go:126-128`

Go map iteration order is random, so the category list passed to the LLM changes between runs. While LLMs should handle this, it could cause non-deterministic categorization results for the same transaction.

**Fix:** Sort the `categories` slice before passing it to the AI.

### 7. `parseCategorySuggestion` doesn't validate the category
**File:** `AI/ai.go:138-158`

The AI might return a category name that isn't in the provided list. The code eventually catches this at the YNAB category map lookup, but only after an unnecessary vector search call. Validating earlier would save an API round-trip.

**Fix:** Pass the valid category list to the parser or add a post-categorization validation step in `categorizeTransaction` before the vector search.

### 8. Migration runner doesn't use transactions
**File:** `migrate/migrate.go:50-58`

Each migration's SQL statements are executed individually without a transaction. If a migration with multiple statements partially fails, the database is left in an inconsistent state, and the migration won't be recorded — but half the DDL has already been applied. This can be especially problematic on re-runs.

**Fix:** Wrap each migration's execution in a `db.BeginTx()` / `tx.Commit()` block (note: DDL transaction support depends on the database engine; verify libSQL/Turso supports transactional DDL).

## Performance

### 9. AI call per transaction is sequential
**File:** `categorizer/categorizer.go:70`

Each transaction is processed serially: AI call, then vector search, then YNAB update. For a batch of N uncategorized transactions, this is N sequential round-trips to the AI API.

**Fix:** Consider batching or parallelizing with a bounded worker pool (e.g., `errgroup` with a semaphore). Be mindful of API rate limits.

### 10. Category map rebuilt every run
**File:** `categorizer/categorizer.go:53`

`refreshCategoryMap()` is called on every `Run()` invocation, which happens every 6 hours by default. This is reasonable for correctness but worth noting — if the interval were shortened significantly, this would add unnecessary YNAB API calls.

**Fix:** Low priority. Could add TTL-based caching if run interval is ever reduced.

### 11. `rows.Err()` not checked after iteration
**File:** `datastore/mapper.go:59-66`

After the `rows.Next()` loop in `FindRelevantContent`, `rows.Err()` should be checked. If the iteration was interrupted by an error, it would be silently swallowed.

**Fix:** Add `if err := rows.Err(); err != nil { return nil, err }` after the loop.

## Test Coverage Gaps

### 12. No tests for the AI package
**Files:** `AI/ai.go`, `AI/vertex.go`

`parseCategorySuggestion` has non-trivial parsing logic (stripping markdown fences, normalizing certainty from 0-100 to 0.0-1.0). This should be tested, especially edge cases like:
- Malformed JSON
- Certainty = 0, certainty = 100
- Missing fields
- Markdown-wrapped responses (`\`\`\`json ... \`\`\``)
- Extra whitespace

**Fix:** Add `AI/ai_test.go` with table-driven tests for `parseCategorySuggestion` and `buildCategorizationPrompt`.

### 13. No tests for `buildCategorizationPrompt`
**File:** `AI/ai.go:119-136`

The prompt construction includes the amount formatting. Negative amounts (which YNAB uses for outflows as milliunits) appear as negative dollar amounts, which could confuse the LLM.

**Fix:** Add tests, and consider formatting amount as absolute value with a direction indicator (e.g., "outflow: $50.00" vs "-$50.00").

### 14. `ynab/client.go` has zero test coverage
**File:** `ynab/client.go`

While it's a thin wrapper, the `UpdateTransactionCategory` payload construction is non-trivial and could be tested with a mock `ClientServicer`.

**Fix:** Add `ynab/client_test.go` with basic tests.

### 15. `migrate/` package has no tests
**File:** `migrate/migrate.go`

The migration runner has statement splitting logic (splitting on `;`) that could break with SQL containing semicolons in strings or comments.

**Fix:** Add tests for the migration runner, particularly for statement splitting edge cases.

## Other Issues

### 16. Duplicated AI provider initialization
**Files:** `main.go:46-58`, `cmd/seed/main.go:42-47`

Near-identical provider initialization logic in both entry points.

**Fix:** Extract to a shared factory function, e.g., `AI.NewFromConfig(cfg)`.

### 17. DB connection string contains auth token in URL
**File:** `main.go:32`

```go
url := fmt.Sprintf("%s?authToken=%s", cfg.LibSQLDatabaseURL, cfg.LibSQLAuthToken)
```

This could appear in logs or error messages. The auth token isn't URL-encoded either, so special characters would break the connection string.

**Fix:** Use `url.Values` to properly encode the query parameter. Consider whether the connection string might appear in error output and redact if so.

### 18. `NewAI` parameter name typo
**File:** `AI/ai.go:27`

```go
func NewAI(otps ...AIOption) (AI, error) {
```

`otps` should be `opts`.

**Fix:** Rename `otps` to `opts`.

### 19. Vertex AI system instruction role is wrong
**File:** `AI/vertex.go:107`

```go
SystemInstruction: genai.NewContentFromText("...", "user"),
```

A system instruction with role `"user"` is semantically wrong. For Vertex AI, system instructions should not specify a user role. This may cause the instruction to be treated as a user message rather than a system prompt, degrading categorization quality.

**Fix:** Check the `google.golang.org/genai` SDK docs for the correct way to set system instructions. Likely should use `"model"` role or a dedicated system instruction method.

## Priority Order

Highest impact fixes first:

1. **#5** — No vector distance threshold (silent mis-categorization risk)
2. **#11** — Missing `rows.Err()` check (silent data loss)
3. **#19** — Vertex system instruction role (incorrect LLM behavior)
4. **#8** — Non-transactional migrations (corrupt DB state risk)
5. **#12** — No AI parsing tests (critical normalization logic untested)
6. **#6** — Non-deterministic category ordering (inconsistent results)
7. **#2** — Fragile embedding serialization (precision loss)
8. **#1** — Dead `ErrNoRows` check (misleading code)
9. **#18** — Parameter typo (readability)
10. **#3** — Value receivers (convention)
11. **#17** — Auth token in URL (security hygiene)
12. **#16** — Duplicated init logic (maintainability)
13. **#7** — No early category validation (wasted API call)
14. **#13-15** — Additional test coverage gaps
15. **#9** — Sequential processing (performance, lower priority for typical batch sizes)
16. **#4** — Invalid Go version in go.mod
17. **#10** — Category map caching (only relevant if interval changes)
