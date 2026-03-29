# Improvements

Code review findings based on README requirements and implementation analysis.

## Bugs

### 1. OpenAI provider will produce wrong-dimension vectors

**Files:** `AI/ai.go:60-63`, `migrations/*.sql`

The database schema correctly uses `F32_BLOB(768)` to match Vertex AI's `text-embedding-004` output. However, OpenAI's `text-embedding-3-small` produces **1536**-dimensional vectors by default. If a user switches to OpenAI, embeddings won't match the schema.

The README also incorrectly states "both produce 1536-dim vectors" — it should say 768.

**Fix:** Set `Dimensions: 768` on the OpenAI `EmbeddingRequest`. The `text-embedding-3-small` model supports arbitrary output dimensions via the API parameter, so this is a one-line fix to ensure compatibility.

### 2. `FindRelevantContent` query has no `ORDER BY` clause

**File:** `datastore/mapper.go:43-45`

The categorizer assumes `results[0]` is the best match and `results[1]` is the second-best (lines 135, 147 of `categorizer.go`). However, the SQL query has no `ORDER BY distance ASC`. While `vector_top_k` may return results in distance order in practice, this is not guaranteed by the SQL semantics — a JOIN can reorder rows.

**Fix:** Add `ORDER BY vk.distance ASC` to the query.

### 3. `cmd/seed` hardcoded to Vertex AI

**File:** `cmd/seed/main.go:51`

The seed command always creates a Vertex AI provider (`AI.NewVertexAI`), ignoring any OpenAI configuration. The README says either provider works, and `main.go` correctly picks based on config. If a user only has an OpenAI API key, seeding will fail.

**Fix:** Use the `config` package (or replicate the provider selection logic from `main.go`) so the seed command respects the same `GOOGLE_CLOUD_PROJECT` / `OPENAI_API_KEY` config.

## Code Quality

### 5. Typo: `descirption` parameter name

**File:** `datastore/mapper.go:26`

`SaveEmbeddings(category, descirption string, ...)` — rename to `description`.

### 6. Unused `model` field in OpenAI provider

**File:** `AI/ai.go:21,31`

The `ai` struct has a `model` field set to `openai.GPT4oMini`, but `GetEmbeddings` hardcodes `"text-embedding-3-small"`. The `model` field is never used. Either use it or remove it.

### 7. `GetTokenCount` is defined but never called

**Files:** `AI/ai.go:12-15`, all callers

The `AI` interface requires `GetTokenCount`, and both providers implement it, but nothing in the codebase calls it. This is dead code on the interface. If it's planned for future use, that's fine — but it currently adds surface area to every provider implementation for no benefit.

### 8. Destructive migration drops all seeded data

**File:** `migrations/2026-03-29-0002-fix-embedding-dimensions.sql`

This migration does `DROP TABLE IF EXISTS searchable_categories` then recreates it. On an existing deployment, this silently destroys all seeded category embeddings, requiring a re-run of `cmd/seed`. The migration runner doesn't track which migrations have been applied — it runs all of them every startup and relies on "already exists" error suppression. This means the DROP will execute on every fresh deploy against an existing database.

**Fix:** Either:
- Use a proper migration tracking table (`schema_migrations`) so each migration runs exactly once, or
- Replace the destructive migration with an `ALTER TABLE` if the schema actually changed (it didn't — both files define `F32_BLOB(768)`).

### 9. `SaveEmbeddings` interface/implementation parameter name mismatch

**Files:** `datastore/mapper.go:12,26`

The `SearchStore` interface defines `SaveEmbeddings(id, content string, ...)` but the implementation signature is `SaveEmbeddings(category, descirption string, ...)`. The caller (`search.go:73`) passes `(id, content, embeddings)` where `id` = category name and `content` = descriptive text. The mapper then stores `id` as the `category` column and `content` as the `description` column, then constructs a *third* `content` column by combining them. This works but is confusing.

**Fix:** Align parameter names across the interface, implementation, and callers. Consider `(category, description string, ...)` everywhere.
