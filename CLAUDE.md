# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
go build -o ynab-autocategorizer .
go test ./...                          # all tests
go test -v ./categorizer               # single package
go test -run TestHighConfidence ./categorizer  # single test
```

Docker:
```bash
docker build -t ynab-autocategorizer .
docker run --env-file .env ynab-autocategorizer
```

Seed the vector database with YNAB categories (one-time setup):
```bash
go run ./cmd/seed
```

## Architecture

YNAB AutoCategorizer fetches uncategorized transactions from YNAB, asks an LLM to pick a category, then validates the LLM's suggestion via vector similarity search against pre-seeded category embeddings in a Turso/libSQL vector database, and updates YNAB.

**Package roles:**
- `categorizer/` — Orchestrator: fetches uncategorized txns, asks AI to categorize, validates via vector search, applies safety checks (AI certainty threshold + ambiguity gap), updates YNAB
- `search/` — Embeds query text via AI provider, queries vector DB for top-K matches
- `AI/` — Provider-agnostic interface with Vertex AI (`vertex.go`) and OpenAI (`ai.go`) implementations; provides both embedding (768-dim vectors) and LLM chat categorization (`CategorizeTransaction`)
- `datastore/` — Turso/libSQL persistence using `vector_top_k` for similarity search against `F32_BLOB(768)` columns
- `ynab/` — Wrapper around `brunomvsouza/ynab.go` SDK for transaction and category operations
- `config/` — Env var loading/validation via godotenv; requires YNAB token + Turso credentials + one AI provider
- `types/` — Shared `SearchResponse` struct (Category, Description, Distance)
- `cmd/seed/` — CLI tool to populate vector DB with category embeddings from YNAB
- `migrate/` — Embedded SQL migration runner with `schema_migrations` tracking

**Key data flow:** `main` → `categorizer.Run()` → for each txn: `AI.CategorizeTransaction(payee, amount, categories)` → `search.Search(suggestion)` (embeds via `AI.GetEmbeddings()` + `datastore.FindRelevantContent()`) → categorizer applies safety checks → `ynab.UpdateTransactionCategory()`

**Categorization safety checks:**
1. Best vector match distance must be ≤ max vector distance threshold (default 0.8); matches above this are skipped
2. AI certainty must be ≥ confidence threshold (default 0.75); transactions below threshold are still categorized but flagged orange
3. Gap between top-2 vector results must be ≥ 10% of best distance (min gap: 0.01); ambiguous matches are flagged orange
4. Vector search must return results and the best match must resolve to a valid YNAB category; otherwise the transaction is skipped

## Testing

All external dependencies are mocked via interfaces (`YNABClient`, `Search`, `AI`, `SearchStore`). Tests cover confidence thresholds, ambiguity detection, dry-run mode, payee/memo fallback, hidden category filtering, and error propagation. No integration tests or external services needed.

## Configuration

All config is via environment variables (loaded from `.env`). See `.env.example` for the full list. Key requirement: must provide either `GOOGLE_CLOUD_PROJECT` (Vertex AI) or `OPENAI_API_KEY`.

`MAX_TRANSACTIONS` caps how many uncategorized transactions are processed per run (0 = unlimited); useful for a cautious first live run.
