# M3 acceptance scripts

Run from the repo root with the real `.env` (it must have `YNAB_SANDBOX_PLAN_ID`, which must differ from
`YNAB_BUDGET_ID`). Nothing here writes to the live plan, clicks in Slack, or posts to Slack. Outputs never carry
secrets, UUIDs, tokens, emails or Slack user ids, and every output file is grepped for them.

> **Rate budget: the sandbox suite shares hoid's YNAB token.** `YNAB_ACCESS_TOKEN` is the token hoid's live service
> syncs with, and YNAB allows 200 requests per token per rolling hour. YNAB sends no header saying how many are left.
> A full `sandbox_check.sh` run makes about 105 requests (about 120 if an earlier run left fixtures to delete), so **never run it twice within an hour**: a 429 starves
> hoid's syncs (stale data, `/healthz` 503, alerts). Three safeguards:
> - **Preflight:** the run starts only if at least 160 requests are left this hour, by its own estimate. The estimate
>   counts every M3 run in `../_m3-acceptance-state/ynab-ledger` (a 429 counts as the whole 200), hoid's own syncs and
>   imports over the last hour from its logs (doubled), and a margin of 10. If not, it exits 4 and runs nothing.
> - **Cap:** the guard caps the whole run at 130 requests. That cap covers every driver process; each `finbot serve`
>   run is charged 5 up front, because serve's requests don't go through the guard.
> - **Stop on 429:** the first 429 stops every later request and is recorded in the ledger.
>
> A separate token for sandbox tests (Brendan's call) would remove the coupling.

| Scope | Command | What it does |
|---|---|---|
| 1. hoid in dry_run | `hoid_check.sh snapshot [dbdir]`, then, after Brendan's click (BA-23), `hoid_check.sh check` | Read-only: copies `finbot.db` and `finbot.db-wal` out of the container (into a remote temp dir that a trap always removes) and reads them with `sqlite3 -readonly`. The container has no sqlite3, so the copy isn't atomic: a copy is used only if `PRAGMA quick_check` passes, with up to 3 tries. Checks `CLASSIFY_MODE=dry_run`, `write_log` empty, decisions all `dry_run` and none `applied`, questions posted, open ≤ cap (5) with the rest pending, no categorize questions, no Slack locks, `dry-run-report` → `writes: 0`. After the click: each snapshotted question answered by a person, its row's category (sha256-compared) and `approved=0` unchanged in a mirror synced after the answer, `write_log` unchanged. For every answered question: answered by a person, row unapproved, and the edited Slack message (read with `conversations.history`) says "YNAB wasn't changed". Exit 3 means it's still waiting for the click or the next sync. The snapshot holds only question numbers and hashes, in `../_m3-acceptance-state/`. |
| 2–7. sandbox | `sandbox_check.sh [workdir]` | Builds `finbot` and the driver, then runs the driver phases below on a fresh DB. Every driver YNAB request goes through a guard that refuses anything but the sandbox plan. The `finbot serve` subprocesses (crash2, mcp, autobin) aren't behind the guard: they're sandbox-only by configuration (`YNAB_BUDGET_ID` is the sandbox, the cwd has no `.env`, `IMPORT_ENABLED=false`, every outward service is blank). The preflight and the hoid lookup run first. Fixtures are `M3acc …` rows in the `finbot M3 acceptance` account, deleted at the start and the end. |

The driver (`driver.go`, `phases.go`, `mcp.go`; `go run ./tests/acceptance/M3 -phase <p> -work <dir> [-bin finbot]`)
wires the real store, syncer (with lock detection), writer, classify pipeline, Slack client and `Questions`, as
serve does. Three things are swapped: Slack's socket and Web API are in-process fakes (a second real Socket Mode
connection would take events from hoid), the model is a fixed fake (`M3acc hi …` scores 0.97, anything else
0.40), and calibrations are built so the hashes match, or are made not to.

| Phase | Scope | Checks |
|---|---|---|
| `writer` | 2 | Apply: done rows, one bulk PATCH, `before_json`/`after_json`, actor/via/endpoint/request id, read back from YNAB. A row a human edited after the decision is refused (`category changed since the decision`), with no write_log row. A human edit made after finbot's write gets a `category/ynab_edit` lock, a later write is refused as `locked`, and the edit holds in YNAB and in the mirror through two more syncs. |
| `slack` | 3 | A click by a user not in `users` changes nothing. A redelivered action, a second click and a race with the other user produce one answer, one write_log row and one PATCH, plus a `category/slack` lock. After a restart (DB reopened, new client, Questions and writer), a click on a message posted before the restart routes and writes once. |
| `mismatch` | 4 | Configured `auto` with an otherwise-matching, auto-eligible calibration whose candidates hash is the live plan's runs as `dry_run` (warning "category list changed since calibration"; gate `dry_run`). A 0.97 row becomes a `dry_run_check` and is not written; a re-decided row's categorize question is superseded; a click on a still-open categorize question is recorded "(dry run)" with no write_log row and no PATCH. |
| `transition` | 5 | `dry_run` posts `dry_run_check`s. Entering `auto` closes them `superseded_by_auto`; a ≥τ row dated today is written (control); a row dated before `CLASSIFY_AUTO_SINCE` is not. Clicks on the closed `dry_run_check`s (gate in auto) do nothing. `writer.Apply` from Slack on the old row returns `ErrBeforeAutoSince`. |
| `crash1` → `crash2` | 6 | crash1 lets the real PATCH reach YNAB, then exits (code 7) before `done`. crash2 checks the row is an `intent` and YNAB has the change, restarts the real `finbot serve` (sandbox as active plan, `CLASSIFY_MODE=off`, everything outward blank) until `/healthz` is 200, stops it, then checks the intent is `done`, the row isn't locked (also after two more syncs), and no intents are left. |
| `mcp` | 7 | A real `finbot serve` with MCP on: 401 without a token, then DCR, a PKCE code exchange (the code is minted in SQLite, standing in for Google sign-in, as in M2), and `get_category_decisions` over Streamable HTTP. It checks the decision count, newest first (applied/auto), oldest dry_run, the category and confidence, `data_freshness`, an empty list for an unknown id, a client error for a malformed id, and no secret or email in the serve log. |
| `autobin` | 4 (extra) | The real binary with `CLASSIFY_MODE=auto`, the shipped calibration and real Jev, on the sandbox: it logs "auto running as dry_run", adds no write_log rows, decides the fixtures in `dry_run`, and they stay uncategorized in YNAB. |

`lib.sh` sources M2's `secret_count` and adds a Slack-user-id count. `slack_read.py` is the read-only Slack reader
for `hoid_check.sh`.
