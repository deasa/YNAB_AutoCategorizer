# finbot

finbot mirrors [YNAB](https://www.ynab.com/) plans into one local SQLite file and keeps the active plan current. It runs
as a single Go binary in one container.

The old AI categorizer is gone. Categorization returns in a later milestone.

## What it does

- **Sync:** a full export the first time a plan is seen, then one delta request every 15 minutes.
- **Archived scan:** `finbot sync --archived` finds every plan with a `Discretionary` category and syncs it.
- **Bank import:** a trigger is built, but it's off by default (`IMPORT_ENABLED=false`). It's a YNAB write, and the only
  one finbot makes.
- **Status:** one Slack message is edited after every scheduled sync and backup, so a stale message means finbot has
  gone quiet. `GET /healthz` reports sync freshness.
- **Backup:** nightly age-encrypted snapshots are stored off-site in Turso, which only ever sees ciphertext. Restore
  runs on a machine holding the private key.

## Quick start

Requires Go 1.26+ and a [YNAB Personal Access Token](https://app.ynab.com/settings/developer). Copy `.env.example` to
`.env` and fill in `YNAB_ACCESS_TOKEN` and `YNAB_BUDGET_ID`. Set `FINBOT_DB_PATH` too if `/data` isn't writable.

```bash
go build -o finbot .

./finbot sync    # one sync of the active plan, then exit
./finbot serve   # daemon: sync on a schedule, /healthz on HTTP_ADDR
```

Commands, architecture, configuration and deployment are in [CLAUDE.md](CLAUDE.md).

## License

MIT
