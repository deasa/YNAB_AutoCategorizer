#!/usr/bin/env bash
# Scopes 2-7 (local, sandbox plan only): runs every driver phase in order on a fresh DB,
# then greps every output (driver output, app logs, the fake Slack's posted text,
# `finbot serve` logs and the DB export) for secrets and ids.
# Usage, from the repo root: tests/acceptance/M3/sandbox_check.sh [workdir]
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
. tests/acceptance/M3/lib.sh
W="${1:-$(mktemp -d)}"; mkdir -p "$W"; chmod 700 "$W"; rm -f "$W"/finbot.db* "$W"/*.log "$W"/crash.json
go build -o "$W/finbot" . && go build -o "$W/m3" ./tests/acceptance/M3 || { echo "FAIL build"; exit 1; }
out="$W/driver.out"; : > "$out"; rc=0
run() { "$W/m3" -work "$W" -bin "$W/finbot" -phase "$@" 2>&1 | tee -a "$out"; return "${PIPESTATUS[0]}"; }
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
