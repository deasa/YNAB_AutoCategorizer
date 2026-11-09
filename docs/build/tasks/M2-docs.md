# M2-docs: apply the M2 docs follow-ups

- Milestone: M2 (gate docs task)
- Branch / worktree: `t/M2-docs`, `../YNAB_AutoCategorizer.wt/M2-docs`
- Depends on: M2-0 .. M2-6 merged (all are)
- Touches (exclusive while running): `CLAUDE.md`, `README.md`, `docs/plan/architecture.md`, `.env.example`
- Plan refs: only the sections the follow-ups name
- YNAB access: none
- Deploy: no

## Goal
CLAUDE.md, README.md, `.env.example` and architecture.md describe main as it is after M2 (MCP + OAuth live behind the
Cloudflare Tunnel, import on in the hoid deploy). This is the one task allowed to edit CLAUDE.md and docs/plan/* for M2.

## Scope
- Apply every item under "Docs follow-ups" in the `## Result` sections of `docs/build/tasks/` M2-0, M2-1a, M2-1b, M2-2,
  M2-3, M2-4, M2-5 and M2-6. Known items, among others:
  - CLAUDE.md package roles for `auth/` and `mcp/`, and the subcommands `finbot revoke` and `finbot tag-account`.
  - Migrations 0004 and 0005 in the `store/` role; `HTTP_ADDR` default `127.0.0.1:8081`.
  - The cutover and rollback scripts and the `tunnel-active` marker, in Build & Run.
  - Backups now hold OAuth state; run `finbot revoke --all` after a restore.
  - `IMPORT_ENABLED` is pinned true in the hoid deploy (M2-6 wording, Build & Run and Safety).
  - architecture.md:48 on plan_scan (M2-0).
  - Old local `.env` files: drop `HTTP_ADDR=:8080` (M2-2).
- README.md: drop the "License: MIT" line (Brendan, BA-14). Add no LICENSE file.
- Check every command, path, env var and default you write against the code on main (grep it). Where a follow-up
  contradicts the code, the code wins: note it under Deviations.

## Out of scope
- Any code, test or deploy change. requirements.md (unless a follow-up names it explicitly).
- New sections or rewrites beyond the follow-ups. Keep CLAUDE.md's style: dense, factual, no ids or secrets.

## Acceptance (each must be demonstrably checked)
- [ ] A table in Result: each follow-up (task, item) → applied at file:line, or skipped with the reason
- [ ] `grep -n "License" README.md` → no "MIT" line
- [ ] Every subcommand in CLAUDE.md's Build & Run exists in `cmd/` dispatch (list them as checked)
- [ ] go build ./... && go vet ./... && go test ./... green (sanity; no code should change)

## Result
- Status: DONE
- What changed:
  - `CLAUDE.md`: Build & Run (two subcommands, cutover/rollback/rehearsal scripts, tunnel marker), `Scheduler` and
    `store/`/`backup/` roles, new `auth/` and `mcp/` roles, Safety, Configuration.
  - `README.md`: MCP bullet, private `/healthz`, import on in the hoid deploy, `finbot revoke`; License section removed.
  - `docs/plan/architecture.md:48`: `plan_scan` semantics.
  - `.env.example`: no change needed; M2-2 already set `HTTP_ADDR=127.0.0.1:8081` and added the MCP keys.
- Follow-ups:

| Task | Item | Outcome |
|---|---|---|
| M2-0 | `0004_plan_scan_keep_verdict` in migrations | CLAUDE.md:77 |
| M2-0 | database files kept 0600 | CLAUDE.md:78 |
| M2-0 | architecture.md `plan_scan` | docs/plan/architecture.md:48 |
| M2-1a | `auth/` package role | CLAUDE.md:96 |
| M2-1a | `0004` and `0005_oauth` in migrations | CLAUDE.md:77-78 |
| M2-1a | backups hold the HMAC secret and token signatures; `revoke --all` after a restore | CLAUDE.md:95 |
| M2-1b | `auth/` covers the server; `go-oidc` and go-sdk | CLAUDE.md:96-97 |
| M2-2 | `mcp/` package role | CLAUDE.md:103 |
| M2-2 | `finbot revoke` | CLAUDE.md:16, README.md:15,31 |
| M2-2 | `HTTP_ADDR` default `127.0.0.1:8081` | CLAUDE.md:146 |
| M2-2 | `MCP_*` and `GOOGLE_OAUTH_*` config | CLAUDE.md:146,155 |
| M2-2 | forwarded by deploy.sh | CLAUDE.md:142, partly: see Deviations |
| M2-2 | prune job in `Scheduler` | CLAUDE.md:66 |
| M2-2 | `/healthz` on its own private listener | CLAUDE.md:70, README.md:17 |
| M2-2 | old `.env`: drop `HTTP_ADDR=:8080` | CLAUDE.md:147 |
| M2-2 | `ActiveTokens` | CLAUDE.md:101-102 |
| M2-2 | `data_freshness` fields and the 14-day rule | CLAUDE.md:107-109 |
| M2-3 | `QueryLines`, `CategoryTotals` in `store/transactions_read.go` | CLAUDE.md:80-81 |
| M2-3 | tools `query_transactions`, `compare_spending`; `badRequest` | CLAUDE.md:106,109 (in the `mcp/` role; there is no MCP section) |
| M2-4 | `finbot tag-account` | CLAUDE.md:17 |
| M2-4 | three new tools | CLAUDE.md:106 |
| M2-4 | `store/snapshot_read.go` | CLAUDE.md:82 |
| M2-4 | window rule | CLAUDE.md:110-111 |
| M2-4 | income rule | CLAUDE.md:82-83 |
| M2-5 | cutover, rollback, remote and rehearsal scripts | CLAUDE.md:39-42,53 |
| M2-5 | `tunnel-active`, `tunnel.env`, `cutover.log` | CLAUDE.md:50-53 |
| M2-5 | Google and allowlist keys forwarded | CLAUDE.md:142 |
| M2-5 | `HTTP_ADDR` on hoid | CLAUDE.md:47-48 |
| M2-6 | Build & Run `IMPORT_ENABLED=true` | CLAUDE.md:47 |
| M2-6 | Safety wording | CLAUDE.md:123-125 |
| Brief | README: drop "License: MIT" (BA-14), no LICENSE file | README.md, section removed |

- Acceptance:
  - Table above → pass.
  - `grep -n "License" README.md` → pass, no output.
  - Every subcommand in Build & Run is in `cmd/app.go` dispatch → pass. Checked: serve, sync (and `--archived`), health,
    backup (and `--list`), restore, revoke, tag-account. Bare `./finbot` is the usage path.
  - `go build ./... && go vet ./... && go test ./...` → pass.
  - Also checked: all seven `deploy/*.sh` exist and are executable; every `MCP_*`, `GOOGLE_OAUTH_*`, `HTTP_*`, `TUNNEL_*`
    name written is in `config`, `cmd` or `deploy`; the diff has no UUIDs, emails or the public hostname.
- Deviations from brief:
  - M2-2 says to add `MCP_*` to deploy.sh's forwarded list. The code forwards only `MCP_ALLOWED_EMAILS`; compose sets
    `MCP_LISTEN_ADDR` and `MCP_PUBLIC_URL`. Code wins, and CLAUDE.md says so.
  - M2-4's Result mentions `tag-account [--list]`. `cmd/tag_account.go` takes no arguments or two, so `--list` exits 2.
    CLAUDE.md documents the real form, matching M2-4's follow-up text.
  - Small additions the follow-ups don't name, needed for the docs to match main: `GOOGLE_OAUTH_CLIENT_SECRET`,
    `TUNNEL_TOKEN` and allowlisted emails in Safety's secrets line (CLAUDE.md:126-128); "HTTP servers" in the shutdown
    sentence; README MCP bullet and the import wording.
- Follow-ups for backlog: none
- Docs follow-ups: none
