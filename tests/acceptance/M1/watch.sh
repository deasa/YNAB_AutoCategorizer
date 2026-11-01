#!/usr/bin/env bash
# Polls hoid logs and the Slack status message so ticks and edits can be lined up afterwards.
# Usage: watch.sh OUTDIR MINUTES. Writes only to OUTDIR.
set -u
OUT=$1; MIN=$2; HERE=$(cd "$(dirname "$0")" && pwd)
set -a; . ./.env; set +a
end=$(( $(date +%s) + MIN*60 ))
while [ "$(date +%s)" -lt "$end" ]; do
  now=$(date -u +%H:%M:%SZ)
  python3 -I "$HERE/slack_status.py" | sed "s/^/$now /" >> "$OUT/slack.log" 2>&1
  ssh -o BatchMode=yes hoid 'docker logs finbot-finbot-1 2>&1' > "$OUT/logs.txt"
  ssh -o BatchMode=yes hoid "docker inspect finbot-finbot-1 --format '{{.State.Health.Status}} streak={{.State.Health.FailingStreak}} restarts={{.RestartCount}}'" | sed "s/^/$now /" >> "$OUT/health.log"
  sleep 300
done
