#!/usr/bin/env bash
# The part of the cutover that runs on hoid; deploy.sh ships it to ~/finbot/src/deploy.
#   precheck           step 2, in the foreground. Nothing public changes.
#   start <timeout>    launches `run` detached, then returns.
#   run <timeout>      steps 3 to 6 and their rollback trap. Survives a dropped ssh
#                      or a sleeping Mac; progress goes to ~/finbot/cutover.log (0600),
#                      ending in one RESULT line. Bodies and logs of the probes and the
#                      connector are never written there, and the token is never read.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$HOME"
dir=finbot
log=$dir/cutover.log
pidfile=$dir/cutover.pid
public_host=finance.hoidlabs.site
curl_image=curlimages/curl:8.17.0
rounds=20
interval=2 # each round also spends about a second in three probes: 20 rounds in about a minute
step=2

is_active() { [[ -f "$pidfile" ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null; }
say() { echo "$(date -u +%H:%M:%SZ) step $step: $*"; }

# Each prints the response body, then the status code on the last line.
probe() {
	docker run --rm "$@"
}
internal_get() {
	probe --network "${dir}_default" --read-only --cap-drop ALL --security-opt no-new-privileges:true "$curl_image" \
		-sS -m 10 -H "Host: $public_host" -w '\n%{http_code}' "http://finbot:8080$1"
}
public_get() {
	probe --read-only --cap-drop ALL --security-opt no-new-privileges:true "$curl_image" \
		-sS -m 10 -w '\n%{http_code}' "https://$public_host$1"
}

# check_site <fetcher>: /mcp is 401, the metadata names finbot exactly, /healthz is 404.
# The spike also answers 401 with the same issuer, so resource_name is what proves
# which stack replied. Bodies are never printed.
check_site() {
	local fetch=$1 resp
	resp=$("$fetch" /mcp) || { echo "/mcp request failed"; return 1; }
	[[ "${resp##*$'\n'}" == 401 ]] || { echo "/mcp returned ${resp##*$'\n'}, want 401"; return 1; }
	resp=$("$fetch" /.well-known/oauth-protected-resource) || { echo "metadata request failed"; return 1; }
	[[ "${resp##*$'\n'}" == 200 ]] || { echo "metadata returned ${resp##*$'\n'}, want 200"; return 1; }
	[[ "$resp" =~ \"resource_name\"[[:space:]]*:[[:space:]]*\"finbot\" ]] ||
		{ echo "metadata resource_name is not finbot"; return 1; }
	resp=$("$fetch" /healthz) || { echo "/healthz request failed"; return 1; }
	[[ "${resp##*$'\n'}" == 404 ]] || { echo "/healthz returned ${resp##*$'\n'}, want 404"; return 1; }
}

precheck() {
	docker pull -q "$curl_image" >/dev/null
	check_site internal_get
	# The spike is serving, so this proves hoid can reach the public URL. Without it
	# a probe problem here would look like a bad cutover at step 5 and roll back.
	local resp
	resp=$(public_get /mcp) || { echo "hoid can't reach the public site"; return 1; }
	[[ "${resp##*$'\n'}" == 401 ]] || { echo "public /mcp returned ${resp##*$'\n'} before cutover, want 401"; return 1; }
}

start() {
	command -v setsid >/dev/null && command -v nohup >/dev/null || { echo "cutover: setsid and nohup are required" >&2; exit 1; }
	if is_active; then echo "cutover: already running on this host" >&2; exit 3; fi
	umask 077
	if [[ -f "$log" ]]; then mv -f "$log" "$log.prev"; fi
	rm -f "$pidfile"
	: >"$log"
	chmod 600 "$log"
	# A new session (setsid) gets no SIGHUP when ssh goes away; nohup covers the rest.
	setsid nohup bash "$here/cutover-remote.sh" run "$1" </dev/null >>"$log" 2>&1 &
}

on_exit() {
	local status=$? result
	trap - EXIT INT TERM
	((status)) || status=1
	say "did not finish; rolling back"
	if bash "$here/rollback-remote.sh" "$timeout" --from-cutover; then
		result="rolled-back step=$step"
	else
		result="rollback-failed step=$step"
		echo "cutover: ROLLBACK FAILED: run deploy/rollback.sh"
	fi
	rm -f "$pidfile"
	echo "RESULT $result"
	exit "$status"
}

run() {
	timeout=$1
	umask 077
	trap '' HUP
	echo $$ >"$pidfile"
	step=3
	trap on_exit EXIT
	trap 'exit 130' INT TERM

	say "start finbot's connector"
	(cd "$dir/src/deploy" && FINBOT_ENV_FILE="$HOME/$dir/finbot.env" TUNNEL_ENV_FILE="$HOME/$dir/tunnel.env" \
		docker compose -p finbot --profile tunnel up -d --no-deps cloudflared)
	local id since deadline registered=
	id=$(docker ps -aq --filter label=com.docker.compose.project=finbot --filter label=com.docker.compose.service=cloudflared)
	[[ -n "$id" ]] || { say "no finbot cloudflared container"; exit 1; }
	# StartedAt has nanosecond precision, so a previous run's lines can't match.
	since=$(docker inspect -f '{{.State.StartedAt}}' "$id")
	deadline=$((SECONDS + timeout))
	while ((SECONDS < deadline)); do
		# Held in a variable (a grep -q pipe would SIGPIPE under pipefail) and never printed.
		logs=$(docker logs --since "$since" "$id" 2>&1 || true)
		if [[ "$logs" == *"Registered tunnel connection"* ]]; then registered=1; break; fi
		sleep 2
	done
	[[ -n "$registered" ]] || { say "no registered connection after ${timeout}s"; exit 1; }

	step=4
	say "stop the spike's connector (its finbot and volume stay)"
	id=$(docker ps -q --filter label=com.docker.compose.project=finbot-spike --filter label=com.docker.compose.service=cloudflared)
	[[ -n "$id" && "$id" != *$'\n'* ]] || { say "expected one running spike connector"; exit 1; }
	docker stop "$id" >/dev/null

	step=5
	say "check the public site $rounds times over a minute"
	local i
	for ((i = 1; i <= rounds; i++)); do
		check_site public_get || { say "public check $i of $rounds failed"; exit 1; }
		((i == rounds)) || sleep "$interval"
	done

	step=6
	say "create the marker"
	touch "$dir/tunnel-active"
	trap - EXIT INT TERM
	rm -f "$pidfile"
	echo "RESULT ok"
}

case "${1:-}" in
precheck) precheck ;;
start) start "$2" ;;
run) run "$2" ;;
*) echo "usage: cutover-remote.sh precheck|start <timeout>|run <timeout>" >&2; exit 2 ;;
esac
