# YNAB AutoCategorizer

Automatically categorize uncategorized [YNAB](https://www.ynab.com/) transactions using AI-powered semantic matching. The app runs on a schedule (default: every 6 hours), fetches your uncategorized transactions, and assigns the best-matching budget category using vector similarity search.

## How It Works

```
┌──────────────┐     ┌───────────────┐     ┌──────────────┐     ┌──────────┐
│  YNAB API    │────▶│  Categorizer  │────▶│  AI Search   │────▶│ Turso DB │
│              │     │               │     │  (Embeddings)│     │ (Vector) │
│ Uncategorized│     │ Match payee   │     │              │     │          │
│ transactions │     │ to category   │     │ Vertex AI /  │     │ libSQL   │
│              │◀────│               │     │ OpenAI       │     │          │
│ Update txn   │     └───────────────┘     └──────────────┘     └──────────┘
│ category     │
└──────────────┘
```

1. **Fetch** uncategorized transactions from YNAB via their [Developer API](https://api.ynab.com/)
2. **Embed** each transaction's payee name using AI (Vertex AI or OpenAI)
3. **Search** the vector database for the closest matching budget category
4. **Update** the transaction in YNAB with the matched category (if confidence exceeds threshold)

## Tech Stack

| Component | Technology |
|---|---|
| Language | Go |
| Database | [Turso](https://turso.tech/) (libSQL) with vector search |
| AI Embeddings | Google Vertex AI (`text-embedding-004`) or OpenAI (`text-embedding-3-small`), 768-dim vectors |
| YNAB API | [YNAB Developer API v1](https://api.ynab.com/) via [`brunomvsouza/ynab.go`](https://github.com/brunomvsouza/ynab.go) |
| Scheduling | Built-in `time.Ticker` (runs every 6 hours by default) |

## Prerequisites

- Go 1.25+
- A [YNAB](https://www.ynab.com/) account with a budget
- A [YNAB Personal Access Token](https://app.ynab.com/settings/developer)
- A [Turso](https://turso.tech/) database
- Google Cloud project with Vertex AI enabled (or an OpenAI API key)

## Configuration

All configuration is via environment variables. Copy `.env.example` to `.env` and fill in your values:

```bash
# YNAB
YNAB_ACCESS_TOKEN=your-ynab-personal-access-token
YNAB_BUDGET_ID=last-used              # or a specific budget UUID

# Turso DB
LIBSQL_DATABASE_URL=libsql://your-db.turso.io
LIBSQL_AUTH_TOKEN=your-turso-auth-token

# AI Provider (Vertex AI)
GOOGLE_CLOUD_PROJECT=your-gcp-project
# GOOGLE_CLOUD_LOCATION=us-central1  # optional, defaults to us-central1

# OR AI Provider (OpenAI)
# OPENAI_API_KEY=your-openai-key

# App Settings
CONFIDENCE_THRESHOLD=0.7              # min similarity to auto-categorize (0.0–1.0)
DRY_RUN=false                         # set to true to log without updating YNAB
RUN_INTERVAL=6h                       # how often to check for new transactions
```

## Getting Started

### 1. Clone & Install

```bash
git clone https://github.com/deasa/YNAB_AutoCategorizer.git
cd YNAB_AutoCategorizer
go mod download
```

### 2. Set Up Your Database

The app automatically runs migrations on startup. Ensure your Turso database URL and auth token are set.

### 3. Seed Categories

Before the auto-categorizer can work, your YNAB budget categories need to be embedded into the vector database:

```bash
go run ./cmd/seed
```

This pulls all categories from your YNAB budget and generates vector embeddings for each one. You only need to do this once (or again if you add new categories).

### 4. Run

```bash
# Load env vars
source .env

# Run the categorizer
go run .
```

The app will:
1. Immediately categorize any uncategorized transactions
2. Then repeat every 6 hours (configurable via `RUN_INTERVAL`)

### 5. Docker (Optional)

```bash
docker build -t ynab-autocategorizer .
docker run --env-file .env ynab-autocategorizer
```

## Project Structure

```
.
├── AI/                  # AI embedding providers (Vertex AI, OpenAI)
│   ├── ai.go            # OpenAI implementation
│   ├── vertex.go        # Vertex AI implementation
│   └── types.go         # Provider-agnostic types
├── categorizer/         # Orchestration — ties YNAB API to search
├── cmd/
│   └── seed/            # CLI to seed category embeddings
├── config/              # Environment configuration
├── datastore/           # Turso DB data access layer
│   └── mapper.go        # Embeddings + state persistence
├── migrations/          # SQL migration files
├── search/              # Vector similarity search service
├── types/               # Shared types
├── ynab/                # YNAB API client wrapper
├── main.go              # Entry point & scheduler
├── Dockerfile
└── .env.example
```

## YNAB API Notes

- **Rate limit**: 200 requests per hour (rolling window). The app uses [delta requests](https://api.ynab.com/#delta-requests) (`server_knowledge`) to minimize API calls.
- **Auth**: Uses a [Personal Access Token](https://api.ynab.com/#personal-access-tokens). Generate one at [YNAB Developer Settings](https://app.ynab.com/settings/developer).
- **Uncategorized filter**: The API supports `?type=uncategorized` to fetch only transactions that need categorization.
- **Budget ID**: Use `last-used` as a shortcut or provide a specific budget UUID.

## How Categories Are Matched

The app uses **vector similarity search** to match transaction payee names to your budget categories:

1. Each budget category name is converted to a high-dimensional vector embedding via AI
2. These embeddings are stored in Turso's libSQL vector index
3. When a new uncategorized transaction arrives, the payee name is embedded and compared against all stored category embeddings
4. The closest match (by vector distance) is evaluated against two safety checks before being assigned

### Safety Checks — When Transactions Stay Uncategorized

The categorizer is intentionally conservative. A transaction will **remain uncategorized** if:

- **Low confidence**: The best match's vector distance exceeds `CONFIDENCE_THRESHOLD` (default: `0.7`). A lower distance means a better match — if the best result is too "far away," the categorizer leaves it for you.
- **Ambiguous match**: The top two category matches have very similar distances (gap < 10% of the best distance). If the app can't clearly distinguish between e.g. "Dining Out" and "Groceries" for a given payee, it won't guess — it leaves it uncategorized so you can decide.

This approach means the categorizer can handle variations in payee names (e.g., "AMZN Mktp US" → "Shopping") without needing exact-match rules, while erring on the side of caution when uncertain.

> **Tip**: Start with `DRY_RUN=true` to see proposed categorizations in the logs without actually changing anything. Tune `CONFIDENCE_THRESHOLD` up (stricter) or down (more permissive) based on the results.

## License

MIT

