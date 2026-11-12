#!/usr/bin/env bash
# Scopes 2-7 (local, sandbox plan only): runs every driver phase in order on a fresh DB,
# then greps every output (driver output, app logs, the fake Slack's posted text,
# `finbot serve` logs and the DB export) for secrets and ids.
# Usage, from the repo root: tests/acceptance/M3/sandbox_check.sh [workdir]
#
# RATE BUDGET: this uses YNAB_ACCESS_TOKEN, the same token hoid's live service syncs with (YNAB allows
# 200 requests per token per rolling hour, and sends no header saying how many are left). A run makes
# about 95 requests. Never run it twice within an hour. A preflight refuses to start (exit 4) unless,
# counting every M3 run in the shared ledger ($STATE/ynab-ledger, a 429 counts as the whole limit),
# hoid's last-hour syncs/imports from its logs and a margin, at least 160 are left. The guard caps the
# whole run at 110 and stops at the first 429.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
. tests/acceptance/M3/lib.sh
W="${1:-$(mktemp -d)}"; mkdir -p "$W"; chmod 700 "$W"; rm -f "$W"/finbot.db* "$W"/*.log "$W"/crash.json
rm -f "$W/requests.count"
STATE="${M3_STATE:-$(cd .. && pwd)/_m3-acceptance-state}"; mkdir -p "$STATE"; chmod 700 "$STATE"
LEDGER="$STATE/ynab-ledger"
go build -o "$W/finbot" . && go build -o "$W/m3" ./tests/acceptance/M3 || { echo "FAIL build"; exit 1; }
out="$W/driver.out"; : > "$out"; rc=0
run() { "$W/m3" -work "$W" -bin "$W/finbot" -ledger "$LEDGER" -phase "$@" 2>&1 | tee -a "$out"; return "${PIPESTATUS[0]}"; }
# hoid's own YNAB use in the last hour (one request per sync tick or import attempt), doubled for safety.
hn=$(ssh hoid 'docker logs --since 60m finbot-finbot-1 2>&1' | grep -cE '"msg":"(sync tick|import tick|import failed|import done)"') \
  || { echo "WAIT can't read hoid's logs to size the rate budget"; exit 4; }
"$W/m3" -work "$W" -ledger "$LEDGER" -hoid-used $((hn * 2)) -phase preflight 2>&1 | tee -a "$out"
[ "${PIPESTATUS[0]}" -eq 0 ] || { echo "SANDBOX RESULT WAIT: rate budget too low; nothing was run"; exit 4; }
for p in cleanup writer slack mismatch transition; do run "$p" || rc=1; done
run crash1; c=$?
if [ "$c" -eq 7 ]; then echo "PASS crash1: process died after the PATCH, before done (exit 7)" | tee -a "$out"; else echo "FAIL crash1 exit $c" | tee -a "$out"; rc=1; fi
for p in crash2 mcp autobin; do run "$p" || rc=1; done
# DB export used in the report: per-table status counts only.
sqlite3 -readonly "$W/finbot.db" "SELECT 'write_log', status, count(*) FROM write_log GROUP BY status;
  SELECT 'questions', kind, status, coalesce(close_reason,''), count(*) FROM questions GROUP BY 2,3,4;
  SELECT 'decisions', mode, outcome, count(*) FROM decisions GROUP BY 2,3;
  SELECT 'human_locks', field, source, count(*) FROM human_locks GROUP BY 2,3;" > "$W/db-export.txt"
cat "$W/db-export.txt"
run cleanup >/dev/null || rc=1
echo "== secret and id grep"
for f in driver.out driver-app.log slack-text.log serve-crash.log serve-mcp.log serve-autobin.log db-export.txt; do
  clean "$W/$f" "$f" || rc=1
done
[ "$rc" -eq 0 ] && echo "SANDBOX RESULT PASS" || echo "SANDBOX RESULT FAIL"
exit "$rc"
