#!/usr/bin/env bash
# Rehearses deploy.sh, cutover.sh and rollback.sh against a fake hoid: stateful
# ssh, docker and curl shims in a throwaway directory, with canary secrets. It
# never reads the real .env, and HOST is a name that can't resolve, so a shim
# that fails to fire can't reach a real machine. Usage: deploy/rehearse.sh
set -uo pipefail
src=$(cd "$(dirname "$0")" && pwd)
base=$(mktemp -d "${TMPDIR:-/tmp}/finbot-rehearse.XXXXXX")
trap 'rm -rf "$base"' EXIT
bin=$base/bin
mkdir -p "$bin"
real_stat=$(command -v stat)
real_sleep=$(command -v sleep)
# SECONDS ticks on the wall clock, so a timeout of 1 can expire at once.
export HOST=rehearsal.invalid TUNNEL_TIMEOUT=3 HEALTH_TIMEOUT=3 POLL_TIMEOUT=30
CANARIES=(CANARY_YNAB_TOKEN CANARY_GOOGLE_SECRET CANARY_TUNNEL_TOKEN canary-allowed@example.invalid)
passed=0 failed=0

cat >"$bin/ssh" <<'EOF'
#!/usr/bin/env bash
# ssh <host> <command...>: runs the command in the fake hoid's home.
# $W/ssh-down makes it fail like a dead link. DROP drops the link around the start:
# after-start once it has returned, during-start before its answer gets back.
echo "ssh $*" >>"$W/argv"
while [[ "$1" == -o ]]; do shift 2; done
shift
[[ -e "$W/ssh-down" ]] && exit 255
(cd "$W/home" && HOME=$W/home bash -c "$*")
rc=$?
if [[ "$*" == *"cutover-remote.sh start"* ]]; then
	case "${DROP:-}" in
	after-start) touch "$W/ssh-down" ;;
	during-start) touch "$W/ssh-down"; exit 255 ;;
	esac
fi
exit "$rc"
EOF
cat >"$bin/stat" <<EOF
#!/usr/bin/env bash
# GNU stat -c %a, as on hoid.
[[ "\$1" == -c ]] && ! "$real_stat" --version >/dev/null 2>&1 && exec "$real_stat" -f %Lp "\$3"
exec "$real_stat" "\$@"
EOF
cat >"$bin/sleep" <<EOF
#!/usr/bin/env bash
exec "$real_sleep" "\${SHIM_SLEEP:-0.02}"
EOF
# macOS has no setsid; this one really does start a new session, as on hoid.
if ! command -v setsid >/dev/null; then
	cat >"$bin/setsid" <<'EOF'
#!/usr/bin/env bash
exec perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV or die' "$@"
EOF
fi
cat >"$bin/site" <<'EOF'
#!/usr/bin/env bash
# site <stack> <path>: the response body, then the status code on the last line.
case "$2" in
/mcp) printf 'unauthorized\n401' ;;
/.well-known/oauth-protected-resource)
	if [[ "$1" == spike ]]; then name='finbot (spike)'; else name=finbot; fi
	printf '{"resource":"https://x","resource_name":"%s"}\n200' "$name" ;;
/healthz) printf 'not found\n404' ;;
*) printf 'nope\n404' ;;
esac
EOF
cat >"$bin/curl" <<'EOF'
#!/usr/bin/env bash
# The Mac must not probe: hoid does it, in a container.
echo "curl $*" >>"$W/argv"
exit 1
EOF
cat >"$bin/public" <<'EOF'
#!/usr/bin/env bash
# The public site, seen from hoid: routed by whichever connectors are running.
[[ "${FAIL:-}" == step2-public ]] && { printf 'tunnel error\n530'; exit 0; }
url=${!#}; path=/${url#https://*/}
echo "$path" >>"$W/public"
up() { [[ "$(cat "$W/c/$1" 2>/dev/null)" == running ]]; }
if up spikecf; then stack=spike
elif up finbotcf; then stack=finbot
else printf 'tunnel error\n530'; exit 0; fi
if [[ "${FAIL:-}" == step5 && "$path" == *oauth-protected* && $(grep -c oauth-protected "$W/public") == 13 ]]; then
	stack=spike
fi
exec site "$stack" "$path"
EOF
cat >"$bin/docker" <<'EOF'
#!/usr/bin/env bash
# A small model of the docker daemon on hoid. Connector count is logged after
# every change so the rehearsal can prove there was never a gap.
echo "docker $*" >>"$W/argv"
count() { local n=0 c; for c in spikecf finbotcf; do [[ "$(cat "$W/c/$c" 2>/dev/null)" == running ]] && n=$((n + 1)); done; echo "$n" >>"$W/timeline"; }
set_state() {
	echo "$2" >"$W/c/$1"; echo "$3 $1" >>"$W/calls"
	[[ "$2" == running ]] && echo "started-$RANDOM-$RANDOM" >"$W/started/$1"
	count
}
cmd=$1; shift
case "$cmd" in
ps)
	all=0 project= service=
	for a in "$@"; do
		case "$a" in
		-aq) all=1 ;;
		label=com.docker.compose.project=*) project=${a##*=} ;;
		label=com.docker.compose.service=*) service=${a##*=} ;;
		esac
	done
	case "$project/$service" in
	finbot-spike/cloudflared) id=spikecf ;; finbot/cloudflared) id=finbotcf ;; finbot/finbot) id=finbotapp ;; *) exit 0 ;;
	esac
	[[ -f "$W/c/$id" ]] || exit 0
	[[ $all == 1 || "$(cat "$W/c/$id")" == running ]] && echo "$id"
	exit 0 ;;
inspect)
	case "$2" in
	*Health*) if [[ "${FAIL:-}" == step1 ]]; then echo starting; else echo healthy; fi ;;
	*StartedAt*) cat "$W/started/$3" ;;
	*) cat "$W/c/$3" ;;
	esac ;;
start) [[ "${FAIL:-}" == spike-start ]] && exit 1; set_state "$1" running start ;;
stop)
	[[ "${FAIL:-}" == step4 && "$1" == spikecf ]] && exit 1
	set_state "$1" exited stop ;;
logs)
	# Only a --since of this run's exact StartedAt excludes the earlier runs' lines.
	id=${!#}
	if [[ "$1" != --since || "$2" != "$(cat "$W/started/$id")" ]]; then echo "old: Registered tunnel connection"; exit 0; fi
	case "${FAIL:-}" in
	step3) [[ "$id" == finbotcf ]] && { echo starting; exit 0; } ;;
	spike-noreg) [[ "$id" == spikecf ]] && { echo starting; exit 0; } ;;
	esac
	echo "Registered tunnel connection" ;;
pull) ;;
run)
	url=${!#}
	[[ "$url" == https://* ]] && exec public "$url"
	path=/${url#http://finbot:8080/}
	stack=finbot; [[ "${FAIL:-}" == step2-name && "$path" == *oauth-protected* ]] && stack=spike
	if [[ "${FAIL:-}" == step2-healthz && "$path" == /healthz ]]; then printf 'ok\n200'; exit 0; fi
	exec site "$stack" "$path" ;;
compose)
	args=" $* "
	[[ "$args" == *" up "* ]] || exit 0
	if [[ "$args" == *" --profile tunnel "* ]]; then
		[[ -r "${TUNNEL_ENV_FILE:-}" ]] || { echo "compose: env file not found" >&2; exit 1; }
		echo "$TUNNEL_ENV_FILE" >>"$W/tunnel_env_file"
		set_state finbotcf running compose-up
	fi
	if [[ "$args" != *" --no-deps "* ]]; then set_state finbotapp running compose-up; fi ;;
*) echo "fake docker: unhandled $cmd" >&2; exit 1 ;;
esac
EOF
chmod +x "$bin"/*

ENV_GOOD='YNAB_ACCESS_TOKEN=CANARY_YNAB_TOKEN
YNAB_BUDGET_ID=CANARY_YNAB_PLAN
GOOGLE_OAUTH_CLIENT_ID=CANARY_GOOGLE_ID
GOOGLE_OAUTH_CLIENT_SECRET=CANARY_GOOGLE_SECRET
MCP_ALLOWED_EMAILS=canary-allowed@example.invalid
UNRELATED_KEY=must-not-ship'

# fresh <name>: a new fake hoid (spike serving) and a clean repo copy of deploy/.
fresh() {
	W=$base/$1
	export W
	mkdir -p "$W/c" "$W/started" "$W/home/finbot-spike/mcp" "$W/repo/deploy"
	: >"$W/argv"; : >"$W/calls"; : >"$W/timeline"; : >"$W/public"; : >"$W/out"
	echo running >"$W/c/spikecf"
	echo started-before-rehearsal >"$W/started/spikecf"
	printf 'TUNNEL_TOKEN=CANARY_TUNNEL_TOKEN\n' >"$W/home/finbot-spike/mcp/tunnel.env"
	chmod 600 "$W/home/finbot-spike/mcp/tunnel.env"
	cp "$src"/compose.yaml "$src"/deploy.sh "$src"/cutover.sh "$src"/cutover-remote.sh "$src"/rollback.sh "$src"/rollback-remote.sh "$W/repo/deploy/"
	(cd "$W/repo" && echo "$ENV_GOOD" >.env && echo .env >.gitignore && git init -q &&
		git add -A && git -c user.name=r -c user.email=r@example.invalid commit -qm rehearsal)
	unset FAIL DROP
}
# run <script>: runs it from the repo copy with the shims first on PATH.
# shellcheck disable=SC2086 # $1 may carry a flag
run() { (cd "$W/repo" && PATH="$bin:$PATH" deploy/$1 >>"$W/out" 2>&1); rc=$?; }
state() { cat "$W/c/$1" 2>/dev/null || echo absent; }
check() { # check <description> <command...>
	local what=$1; shift
	if "$@"; then passed=$((passed + 1)); else failed=$((failed + 1)); echo "  FAIL: $what"; sed 's/^/    | /' "$W/out" | tail -8; fi
}
is() { [[ "$1" == "$2" ]]; }
exists() { [[ -e "$W/home/$1" ]]; }
absent() { [[ ! -e "$W/home/$1" ]]; }
called() { grep -qE "$1" "$W/argv"; }
not_called() { ! grep -qE "$1" "$W/argv"; }
no_gap() { ! grep -qx 0 "$W/timeline"; }
out_has() { grep -q "$1" "$W/out"; }
before() { # the first calls-line matching $1 comes before the first matching $2
	local a b
	a=$(grep -n "$1" "$W/calls" | head -1 | cut -d: -f1)
	b=$(grep -n "$2" "$W/calls" | head -1 | cut -d: -f1)
	[[ -n "$a" && -n "$b" && "$a" -lt "$b" ]]
}
spike_only() { is "$(state spikecf)" running && ! is "$(state finbotcf)" running; }
never_started_finbot_cf() { is "$(state finbotcf)" absent && spike_only; }
fail_ran_no_rollback() { ! out_has 'rolling back'; }
hoid_log=finbot/cutover.log
result_on_hoid() { # result_on_hoid <text>: wait for hoid's own RESULT line, with no help from the Mac
	local _
	for _ in $(seq 1 400); do grep -q "^RESULT" "$W/home/$hoid_log" 2>/dev/null && break; "$real_sleep" 0.05; done
	grep -q "^RESULT $1" "$W/home/$hoid_log"
}
wait_pid() { # wait until the detached run has written its pid
	local _
	for _ in $(seq 1 400); do [[ -s "$W/home/finbot/cutover.pid" ]] && break; "$real_sleep" 0.02; done
	cat "$W/home/finbot/cutover.pid"
}

echo "== happy path, every step"
fresh happy; run cutover.sh
check "exits 0" is "$rc" 0
check "step 0 copied tunnel.env" exists finbot/tunnel.env
check "tunnel.env is 0600" is "$("$bin/stat" -c %a "$W/home/finbot/tunnel.env")" 600
for s in 0 1 2 3 4 5 6; do check "ran step $s" out_has "step $s"; done
check "step 2 probe uses the public Host on finbot's network" called 'run --rm --network finbot_default .*Host: finance.hoidlabs.site'
check "connector started before spike stopped" before 'compose-up finbotcf' 'stop spikecf'
check "spike's connector stopped" is "$(state spikecf)" exited
check "finbot's connector running" is "$(state finbotcf)" running
check "spike's finbot container and volume untouched" not_called 'docker (rm|volume|stop finbot-spike_finbot)'
check "20 public metadata checks" is "$(grep -c oauth-protected "$W/public")" 20
check "20 public /mcp checks, plus the one before cutover" is "$(grep -c '/mcp$' "$W/public")" 21
check "20 public /healthz checks" is "$(grep -c healthz "$W/public")" 20
check "marker created" exists finbot/tunnel-active
check "never a moment with no connector" no_gap
check "the Mac never probed the public site" not_called '^curl '
check "steps 3 to 6 were started detached" called 'cutover-remote.sh start'
check "cutover.log is 0600" is "$("$bin/stat" -c %a "$W/home/$hoid_log")" 600
check "cutover.log ends in the result" is "$(tail -1 "$W/home/$hoid_log")" "RESULT ok"
check "no pid file left" absent finbot/cutover.pid
check "no rollback" fail_ran_no_rollback
check "finbot.env has the MCP keys" grep -q '^GOOGLE_OAUTH_CLIENT_SECRET=' "$W/home/finbot/finbot.env"
check "finbot.env has the allowlist" grep -q '^MCP_ALLOWED_EMAILS=' "$W/home/finbot/finbot.env"
check "finbot.env has no tunnel token" bash -c '! grep -q TUNNEL_TOKEN "$0"' "$W/home/finbot/finbot.env"
check "finbot.env has no unrelated key" bash -c '! grep -q UNRELATED_KEY "$0"' "$W/home/finbot/finbot.env"
run cutover.sh
check "second cutover refuses" is "$rc" 1
check "second cutover says why" out_has 'already cut over'
: >"$W/calls"; : >"$W/timeline"; run rollback.sh
check "one-command rollback exits 0" is "$rc" 0
check "rollback leaves only the spike's connector" spike_only
check "rollback removes the marker" absent finbot/tunnel-active
check "rollback starts spike before stopping finbot's" before 'start spikecf' 'stop finbotcf'
check "rollback never leaves a gap" no_gap

echo "== rollback when the spike can't take over"
for mode in spike-noreg spike-start; do
	fresh "rb-$mode"; run cutover.sh; FAIL=$mode; export FAIL; run rollback.sh; unset FAIL
	check "$mode: rollback fails loudly" is "$rc" 1
	check "$mode: finbot's connector left running" is "$(state finbotcf)" running
	check "$mode: marker kept" exists finbot/tunnel-active
	check "$mode: no gap" no_gap
done

echo "== step 0 failures change nothing"
fresh s0-missing; rm "$W/home/finbot-spike/mcp/tunnel.env"; run cutover.sh
check "missing source fails" is "$rc" 1
fresh s0-twolines; printf 'TUNNEL_TOKEN=CANARY_TUNNEL_TOKEN\nTUNNEL_TOKEN=CANARY_TUNNEL_TOKEN\n' >"$W/home/finbot-spike/mcp/tunnel.env"; run cutover.sh
check "two token lines fails" is "$rc" 1
fresh s0-empty; printf 'TUNNEL_TOKEN=\n' >"$W/home/finbot-spike/mcp/tunnel.env"; run cutover.sh
check "empty token fails" is "$rc" 1
fresh s0-spikedown; echo exited >"$W/c/spikecf"; run cutover.sh
check "spike connector down fails" is "$rc" 1
for s in s0-missing s0-twolines s0-empty; do
	W=$base/$s
	check "$s: nothing deployed" not_called 'docker compose'
	check "$s: no tunnel.env left behind" absent finbot/tunnel.env
	check "$s: no temp file left behind" absent finbot/tunnel.env.new
	check "$s: spike untouched" never_started_finbot_cf
done

echo "== forced failures before the trap arm: spike keeps serving"
for mode in step1 step2-name step2-healthz step2-public; do
	fresh "$mode"; FAIL=$mode; export FAIL; run cutover.sh; unset FAIL
	check "$mode: exits nonzero" is "$rc" 1
	check "$mode: spike's connector still running, finbot's never started" never_started_finbot_cf
	check "$mode: spike never stopped" not_called 'docker stop'
	check "$mode: no rollback ran" fail_ran_no_rollback
	check "$mode: no marker" absent finbot/tunnel-active
	check "$mode: no gap" no_gap
	check "$mode: nothing detached" not_called 'cutover-remote.sh start'
done

echo "== forced failures after the trap arm: rollback leaves exactly the spike's connector"
for mode in step3 step4 step5; do
	fresh "$mode"; FAIL=$mode; export FAIL; run cutover.sh; unset FAIL
	check "$mode: exits nonzero" is "$rc" 1
	check "$mode: rollback ran" out_has 'rolling back'
	check "$mode: exactly the spike's connector" spike_only
	check "$mode: no marker" absent finbot/tunnel-active
	check "$mode: no gap" no_gap
done
W=$base/step5
check "step5: spike restarted before finbot's stopped" before 'start spikecf' 'stop finbotcf'
check "step5: failure names the check" out_has 'public check 13 of 20'

echo "== detached run: a HUP is ignored, a TERM rolls back"
fresh term
export SHIM_SLEEP=0.2
(cd "$W/repo" && PATH="$bin:$PATH" exec deploy/cutover.sh >>"$W/out" 2>&1) &
mac=$!
pid=$(wait_pid)
check "runs in its own session, not ssh's" is "$(ps -o pgid= -p "$pid" | tr -d ' ')" "$pid"
for _ in $(seq 1 400); do [[ $(wc -l <"$W/public") -ge 5 ]] && break; "$real_sleep" 0.05; done
kill -HUP "$pid"; "$real_sleep" 0.5
check "survives SIGHUP" kill -0 "$pid"
kill -TERM "$pid"; wait "$mac"; rc=$?
unset SHIM_SLEEP
check "Mac reports rolled back" is "$rc" 1
check "rollback ran" out_has 'rolling back'
check "exactly the spike's connector" spike_only
check "no marker" absent finbot/tunnel-active
check "no gap" no_gap

echo "== a simulated ssh drop: hoid finishes alone"
fresh drop-ok; export DROP=after-start POLL_TIMEOUT=1; run cutover.sh; unset DROP; export POLL_TIMEOUT=30
check "drop after start: Mac reports unknown" is "$rc" 3
check "drop after start: says to re-attach" out_has 'cutover.sh --watch'
check "drop after start: hoid finishes" result_on_hoid ok
check "drop after start: finbot's connector serves" is "$(state finbotcf)" running
check "drop after start: spike's stopped" is "$(state spikecf)" exited
check "drop after start: marker created" exists finbot/tunnel-active
check "drop after start: no gap" no_gap
rm "$W/ssh-down"; : >"$W/out"; run "cutover.sh --watch"
check "--watch after the link returns: exit 0" is "$rc" 0
check "--watch shows the whole log" out_has 'step 3'

fresh drop-step5; export FAIL=step5 DROP=after-start POLL_TIMEOUT=1; run cutover.sh; unset DROP; export POLL_TIMEOUT=30
check "drop plus step 5 failure: Mac reports unknown" is "$rc" 3
check "drop plus step 5 failure: hoid rolls back alone" result_on_hoid 'rolled-back step=5'
unset FAIL
check "drop plus step 5 failure: exactly the spike's connector" spike_only
check "drop plus step 5 failure: no marker" absent finbot/tunnel-active
check "drop plus step 5 failure: spike up before finbot's down" before 'start spikecf' 'stop finbotcf'
check "drop plus step 5 failure: no gap" no_gap
rm "$W/ssh-down"; : >"$W/out"; run "cutover.sh --watch"
check "--watch reports the rollback" is "$rc" 1

fresh drop-during; export DROP=during-start POLL_TIMEOUT=1; run cutover.sh; unset DROP; export POLL_TIMEOUT=30
check "drop during start: not treated as a start failure" is "$rc" 3
check "drop during start: hoid finishes" result_on_hoid ok
check "drop during start: marker created" exists finbot/tunnel-active

fresh mac-gone
(cd "$W/repo" && PATH="$bin:$PATH" exec deploy/cutover.sh >>"$W/out" 2>&1) &
mac=$!
wait_pid >/dev/null
kill -KILL "$mac"; wait "$mac" 2>/dev/null
check "Mac killed mid-run: hoid finishes" result_on_hoid ok
check "Mac killed mid-run: marker created" exists finbot/tunnel-active

echo "== one cutover at a time"
fresh busy; mkdir -p "$W/home/finbot"; "$real_sleep" 60 & holder=$!
echo "$holder" >"$W/home/finbot/cutover.pid"
run rollback.sh
check "rollback.sh refuses during a cutover" is "$rc" 1
check "rollback.sh says how to stop it" out_has 'kill -TERM'
check "that rollback changed nothing" is "$(state spikecf)" running
run cutover.sh
check "a second cutover won't start" is "$rc" 1
check "a second cutover says why" out_has 'already running'
kill "$holder"; wait "$holder" 2>/dev/null
run rollback.sh
check "a stale pid file doesn't block rollback" is "$rc" 0

echo "== deploy.sh and the marker"
marker() { mkdir -p "$W/home/finbot"; touch "$W/home/finbot/tunnel-active"; }
token() { printf "$1" >"$W/home/finbot/tunnel.env"; chmod "$2" "$W/home/finbot/tunnel.env"; }
fresh d-nomarker; run deploy.sh
check "no marker: deploy succeeds" is "$rc" 0
check "no marker: no profile" not_called 'profile tunnel'
check "no marker: no connector" is "$(state finbotcf)" absent
check "no marker: finbot up" is "$(state finbotapp)" running
fresh d-missing; marker; run deploy.sh
check "marker, tunnel.env missing: fails" is "$rc" 1
check "marker, tunnel.env missing: loud" out_has 'FAILED.*tunnel.env'
check "marker, tunnel.env missing: nothing started" is "$(state finbotapp)" absent
fresh d-0644; marker; token 'TUNNEL_TOKEN=CANARY_TUNNEL_TOKEN\n' 644; run deploy.sh
check "marker, 0644: fails" is "$rc" 1
check "marker, 0644: loud" out_has 'FAILED.*tunnel.env'
check "marker, 0644: nothing started" is "$(state finbotapp)" absent
fresh d-two; marker; token 'TUNNEL_TOKEN=CANARY_TUNNEL_TOKEN\nTUNNEL_TOKEN=CANARY_TUNNEL_TOKEN\n' 600; run deploy.sh
check "marker, two lines: fails" is "$rc" 1
fresh d-none; marker; token 'OTHER=x\n' 600; run deploy.sh
check "marker, no token line: fails" is "$rc" 1
fresh d-empty; marker; token 'TUNNEL_TOKEN=\n' 600; run deploy.sh
check "marker, empty token: fails" is "$rc" 1
fresh d-good; marker; token 'TUNNEL_TOKEN=CANARY_TUNNEL_TOKEN\n' 600; run deploy.sh
check "marker, valid: succeeds" is "$rc" 0
check "marker, valid: tunnel profile on" called 'profile tunnel'
check "marker, valid: connector running" is "$(state finbotcf)" running
for key in GOOGLE_OAUTH_CLIENT_ID GOOGLE_OAUTH_CLIENT_SECRET MCP_ALLOWED_EMAILS; do
	fresh "d-no-$key"; grep -v "^$key=" "$W/repo/.env" >"$W/e" && cp "$W/e" "$W/repo/.env"; run deploy.sh
	check "$key absent: fails" is "$rc" 1
	check "$key absent: loud" out_has "no value for $key"
	check "$key absent: nothing shipped" not_called 'docker compose'
	fresh "d-blank-$key"; sed -i.bak "s|^$key=.*|$key=\"\"|" "$W/repo/.env"; run deploy.sh
	check "$key blank: fails" is "$rc" 1
done

echo "== secrets"
hits=0
for c in "${CANARIES[@]}"; do
	n=$(cat "$base"/*/out "$base"/*/argv "$base"/*/home/finbot/cutover.log* 2>/dev/null | grep -c -- "$c")
	hits=$((hits + n))
done
if [[ $hits == 0 ]]; then passed=$((passed + 1)); else failed=$((failed + 1)); echo "  FAIL: $hits canary hits in output or argv"; fi
echo "canary hits over all script output and argv: $hits"

echo "passed=$passed failed=$failed"
[[ $failed == 0 ]]
