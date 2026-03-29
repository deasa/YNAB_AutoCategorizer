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

YNAB AutoCategorizer fetches uncategorized transactions from YNAB, generates vector embeddings for payee names, performs similarity search against pre-seeded category embeddings in a Turso/libSQL vector database, and updates YNAB when a confident match is found.

**Package roles:**
- `categorizer/` — Orchestrator: fetches uncategorized txns, runs search, applies safety checks (confidence threshold + ambiguity gap), updates YNAB
- `search/` — Embeds query text via AI provider, queries vector DB for top-K matches
- `AI/` — Provider-agnostic embedding interface with Vertex AI (`vertex.go`) and OpenAI (`ai.go`) implementations; both produce 768-dim vectors
- `datastore/` — Turso/libSQL persistence using `vector_top_k` for similarity search against `F32_BLOB(768)` columns
- `ynab/` — Wrapper around `brunomvsouza/ynab.go` SDK for transaction and category operations
- `config/` — Env var loading/validation via godotenv; requires YNAB token + Turso credentials + one AI provider
- `types/` — Shared `SearchResponse` struct (Category, Description, Distance)
- `cmd/seed/` — CLI tool to populate vector DB with category embeddings from YNAB

**Key data flow:** `main` → `categorizer.Run()` → for each txn: `search.Search(payee)` → `AI.GetEmbeddings()` + `datastore.FindRelevantContent()` → categorizer applies threshold/ambiguity checks → `ynab.UpdateTransactionCategory()`

**Categorization safety checks:**
1. Best match distance must be ≤ confidence threshold (default 0.7; lower distance = better match)
2. Gap between top-2 results must be ≥ 10% of best distance (min gap: 0.01) to avoid ambiguous matches

## Testing

All external dependencies are mocked via interfaces (`YNABClient`, `Search`, `AI`, `SearchStore`). Tests cover confidence thresholds, ambiguity detection, dry-run mode, payee/memo fallback, hidden category filtering, and error propagation. No integration tests or external services needed.

## Configuration

All config is via environment variables (loaded from `.env`). See `.env.example` for the full list. Key requirement: must provide either `GOOGLE_CLOUD_PROJECT` (Vertex AI) or `OPENAI_API_KEY`.
