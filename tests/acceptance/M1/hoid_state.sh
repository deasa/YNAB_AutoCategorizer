#!/usr/bin/env bash
# Item 1, 2 and 6: read-only view of the finbot container on hoid. Never prints env values
# other than IMPORT_ENABLED. Usage: hoid_state.sh > out; then run secret_count on out.
set -u
C=finbot-finbot-1
ssh -o BatchMode=yes hoid bash -s <<REMOTE
echo "== ps"; docker ps -a --filter label=com.docker.compose.project=finbot --format '{{.Names}} | {{.Status}} | ports=[{{.Ports}}] | {{.Image}}'
echo "== inspect"
docker inspect $C --format 'project={{index .Config.Labels "com.docker.compose.project"}}
workdir={{index .Config.Labels "com.docker.compose.project.working_dir"}}
user={{.Config.User}}
readonly={{.HostConfig.ReadonlyRootfs}}
privileged={{.HostConfig.Privileged}}
capdrop={{.HostConfig.CapDrop}}
capadd={{.HostConfig.CapAdd}}
secopt={{.HostConfig.SecurityOpt}}
portbindings={{.HostConfig.PortBindings}}
publishall={{.HostConfig.PublishAllPorts}}
tmpfs={{.HostConfig.Tmpfs}}
restart={{.HostConfig.RestartPolicy.Name}}
restartcount={{.RestartCount}}
started={{.State.StartedAt}}
health={{.State.Health.Status}}
healthfails={{.State.Health.FailingStreak}}
logcfg={{.HostConfig.LogConfig.Type}} {{.HostConfig.LogConfig.Config}}
image_id={{.Image}}
mounts={{range .Mounts}}{{.Type}}:{{.Name}}:{{.Destination}}:rw={{.RW}} {{end}}
networks={{range \$k,\$v := .NetworkSettings.Networks}}{{\$k}} {{end}}'
echo "== env names"; docker inspect $C --format '{{range .Config.Env}}{{println .}}{{end}}' | sed 's/=.*//' | sort | tr '\n' ' '; echo
echo "== import env"; docker inspect $C --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -E '^(IMPORT_ENABLED|IMPORT_INTERVAL|SYNC_INTERVAL|TZ|GIT_SHA)=' || echo none
echo "== id"; docker exec $C id
echo "== caps"; docker exec $C grep -E '^(Cap(Eff|Prm|Bnd)|NoNewPrivs)' /proc/1/status
echo "== top"; docker top $C -eo uid,pid,args
echo "== healthz"; docker inspect $C --format '{{(index .State.Health.Log 0).ExitCode}}'
echo "== usage"; docker exec $C finbot 2>&1 | head -20
REMOTE
