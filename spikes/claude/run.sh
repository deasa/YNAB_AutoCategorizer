#!/usr/bin/env bash
# M0-4b runner, executed ON hoid from ~/finbot-spike/. Usage (prompt on stdin):
#   [EXTRA_ENV="K=V ..."] [CWD=dir] ./run.sh <name> <claude args...>
# Loads CLAUDE_CODE_OAUTH_TOKEN from ~/finbot/.env without printing it, runs claude
# from an empty scratch cwd, saves stdout to out/<name>.out, prints exit code +
# wall-clock ms, and bumps the call counter in calls.txt.
set -u
cd "$(dirname "$0")"
name=$1; shift
mkdir -p out work
set -a; . ~/finbot/.env; set +a
[ -n "${BAD_TOKEN:-}" ] && CLAUDE_CODE_OAUTH_TOKEN=$BAD_TOKEN
unset ANTHROPIC_API_KEY
export DISABLE_UPDATES=1
echo "$name" >> calls.txt
start=$(date +%s%N)
( cd "${CWD:-work}" && env ${EXTRA_ENV:-} timeout "${TIMEOUT:-120}" ~/.local/bin/claude "$@" ) \
  > "out/$name.out" 2> "out/$name.err"
rc=$?
ms=$(( ($(date +%s%N) - start) / 1000000 ))
echo "$name rc=$rc ms=$ms" | tee -a out/timings.txt
