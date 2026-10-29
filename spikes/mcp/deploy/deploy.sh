#!/bin/sh
# Deploys the spike to hoid:~/finbot-spike/mcp. Run from the repo root.
# Secrets are streamed over ssh stdin and written 0600; they never touch argv or stdout.
set -eu
host=${HOST:-hoid}
dir=finbot-spike/mcp

ssh "$host" "mkdir -p $dir/src && chmod 700 finbot-spike $dir"
rsync -a --delete --exclude .data --exclude mcp-spike --exclude deploy spikes/mcp/ "$host:$dir/src/"
rsync -a spikes/mcp/deploy/compose.yaml "$host:$dir/compose.yaml"

# mcp.env: only the three keys finbot needs, from the local .env.
grep -E '^(GOOGLE_OAUTH_CLIENT_ID|GOOGLE_OAUTH_CLIENT_SECRET|MCP_ALLOWED_EMAILS)=' .env |
	ssh "$host" "umask 077 && cat > $dir/mcp.env"

# tunnel.env: only the tunnel token, renamed to what cloudflared reads.
ssh "$host" "umask 077 && sed -n 's/^CLOUDFLARE_TUNNEL_TOKEN=/TUNNEL_TOKEN=/p' finbot/.env > $dir/tunnel.env"

ssh "$host" "cd $dir && docker compose up -d --build 2>&1 | tail -5"
