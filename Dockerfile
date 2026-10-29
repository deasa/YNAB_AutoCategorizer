# Build stage
FROM golang:1.26.10-bookworm AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /finbot .

# Runtime stage: glibc (Debian slim), because the claude CLI (M3) needs it.
FROM debian:12.15-slim

RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates tzdata \
	&& rm -rf /var/lib/apt/lists/* \
	&& useradd --system --uid 10001 --user-group --home-dir /data --no-create-home finbot \
	&& install -d -o finbot -g finbot -m 0700 /data
COPY --from=builder /finbot /usr/local/bin/finbot

USER finbot:finbot
WORKDIR /data
VOLUME /data
ENV FINBOT_DB_PATH=/data/finbot.db

ENTRYPOINT ["finbot"]
CMD ["serve"]
