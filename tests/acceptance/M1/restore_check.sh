#!/usr/bin/env bash
# Item 5: restore the newest backup into a temp dir, check integrity and row counts, try a wrong
# key, delete everything. Usage: restore_check.sh FINBOT_BIN KEY_PATH WORKDIR
# Prints counts only. Slack is blanked; restore has no reason to touch it.
set -u
BIN=$1; KEY=$2; W=$(mktemp -d "$3/restore.XXXX")
trap 'rm -rf "$W"; echo "cleanup: restored files removed: $([ -e "$W" ] && echo NO || echo yes)"' EXIT
export SLACK_BOT_TOKEN=
echo "== restore newest"; "$BIN" restore --key "$KEY" --out "$W/r.db" 2>&1; echo "exit=$?"
ls -l "$W/r.db" | awk '{print "mode="$1" bytes="$5}'
if [ -f "$W/r.db" ]; then
  echo "header=$(head -c 15 "$W/r.db")"
  echo "integrity=$(sqlite3 "file:$W/r.db?mode=ro" 'PRAGMA integrity_check;')"
  for t in $(sqlite3 "file:$W/r.db?mode=ro" "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY 1;"); do
    echo "$t=$(sqlite3 "file:$W/r.db?mode=ro" "SELECT count(*) FROM \"$t\";")"
  done
  echo "sync_state_rows=$(sqlite3 "file:$W/r.db?mode=ro" 'SELECT count(*) FROM sync_state;')"
fi
echo "== wrong key must fail and leave nothing"
(cd "$(dirname "$0")/../../.." && go run filippo.io/age/cmd/age-keygen -o "$W/wrong.key") >/dev/null 2>&1 || echo "keygen unavailable"
if [ -f "$W/wrong.key" ]; then
  "$BIN" restore --key "$W/wrong.key" --out "$W/w.db" >"$W/w.out" 2>&1; echo "exit=$?"
  echo "w.db exists: $([ -e "$W/w.db" ] && echo YES || echo no); leftover files: $(ls "$W" | grep -c '^w\.db')"
  head -c 200 "$W/w.out"; echo
fi
