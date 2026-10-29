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

- Go 1.26+
- A [YNAB Personal Access Token](https://app.ynab.com/settings/developer)

## Configuration

All configuration is via environment variables. Copy `.env.example` to `.env` and fill in your values:

```bash
YNAB_ACCESS_TOKEN=your-ynab-personal-access-token   # required
YNAB_BUDGET_ID=your-active-plan-id                  # required: the active plan
FINBOT_DB_PATH=/data/finbot.db                      # SQLite mirror
SYNC_INTERVAL=15m                                   # serve: delta-sync cadence
IMPORT_INTERVAL=6h                                  # serve: bank-import cadence
IMPORT_ENABLED=false                                # the import is a YNAB write; off by default
HTTP_ADDR=:8080                                     # serve: /healthz listener
LOG_LEVEL=info                                      # debug, info, warn, error
```

## Running

```bash
go build -o finbot .

./finbot sync              # one delta sync of the active plan (a full export the first time), then exit
./finbot sync --archived   # scan every plan for a Discretionary category, then sync each plan that has it
./finbot serve             # daemon: sync now and every SYNC_INTERVAL; GET /healthz on HTTP_ADDR
```

`/healthz` answers 200 while a sync has succeeded within 2×`SYNC_INTERVAL`, otherwise 503.

### Docker

```bash
docker build -t finbot .
docker run --env-file .env -v finbot-data:/data finbot serve
```

## Project Structure

```
.
├── cmd/       # finbot subcommands: serve (scheduler + /healthz), sync
├── config/    # environment configuration
├── store/     # SQLite mirror (modernc.org/sqlite) with embedded migrations
├── sync/      # delta sync, archived-plan scan, bank-import trigger
├── ynab/      # YNAB API client wrapper (rate governor, error redaction)
├── main.go    # entry point
└── Dockerfile
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

