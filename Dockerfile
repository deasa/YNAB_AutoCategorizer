# Build stage
FROM golang:1.26.10-bookworm AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG GIT_SHA=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
	-ldflags "-X github.com/deasa/YNAB_AutoCategorizer/cmd.Version=${GIT_SHA}" -o /finbot .

# Runtime stage: glibc (Debian slim), because the claude CLI (M3) needs it.
FROM debian:12.15-slim

RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates tzdata \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd --system --uid 10001 --user-group --home-dir /data --no-create-home finbot \
	&& install -d -o finbot -g finbot -m 0700 /data

# The pinned claude CLI. The installer writes under $HOME, and finbot's home is /data,
# which the finbot-data volume mounts over, so it runs as root with HOME=/opt/claude.
# Only the versioned binary is kept (root-owned, world-readable); the installer's
# state (.claude/, .claude.json, launcher) is dropped, since CLAUDE_CONFIG_DIR puts the
# runtime state on the volume. The installer is the reviewed one: a changed script
# fails the build. It verifies the binary's sha256 against the release manifest.
ARG CLAUDE_VERSION=2.1.312
ARG CLAUDE_INSTALLER_SHA256=3a68d3406cf674e17bed1733a4dcf37805e2e47d87417700007d7e1aa766a944
# DISABLE_UPDATES is set only after this step: with it, `claude install` refuses and
# installs nothing.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends curl \
	&& curl -fsSL https://claude.ai/install.sh -o /tmp/install.sh \
	&& echo "${CLAUDE_INSTALLER_SHA256}  /tmp/install.sh" | sha256sum -c - \
	&& HOME=/opt/claude bash /tmp/install.sh "${CLAUDE_VERSION}" \
	&& bin="/opt/claude/.local/share/claude/versions/${CLAUDE_VERSION}" \
	&& test -f "$bin" \
	&& install -d -m 0755 /opt/claude/versions \
	&& install -m 0755 "$bin" "/opt/claude/versions/${CLAUDE_VERSION}" \
	&& rm -rf /tmp/install.sh /opt/claude/.local /opt/claude/.claude /opt/claude/.claude.json /opt/claude/.cache \
	&& ln -s "/opt/claude/versions/${CLAUDE_VERSION}" /usr/local/bin/claude \
	&& apt-get purge -y --auto-remove curl \
	&& rm -rf /var/lib/apt/lists/* \
	&& test "$(DISABLE_UPDATES=1 HOME=/tmp /usr/local/bin/claude --version | cut -d' ' -f1)" = "${CLAUDE_VERSION}" \
	&& rm -rf /tmp/.claude /tmp/.claude.json /tmp/.cache /tmp/.local
ENV DISABLE_UPDATES=1
COPY --from=builder /finbot /usr/local/bin/finbot

USER finbot:finbot
WORKDIR /data
VOLUME /data
ENV FINBOT_DB_PATH=/data/finbot.db

ENTRYPOINT ["finbot"]
CMD ["serve"]
