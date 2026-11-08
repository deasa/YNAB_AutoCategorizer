#!/usr/bin/env bash
# One-command rollback to the finbot-spike connector, from the Mac. Safe to re-run.
# The logic is rollback-remote.sh, streamed over ssh so it works whatever is in ~/finbot/src.
set -euo pipefail
host=${HOST:-hoid}
timeout=${TUNNEL_TIMEOUT:-90}
ssh "$host" bash -s -- "$timeout" <"$(dirname "$0")/rollback-remote.sh"
