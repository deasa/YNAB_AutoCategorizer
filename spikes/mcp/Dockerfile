# Multi-stage build for the MCP spike. Built on hoid from a copy of spikes/mcp.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mcp-spike . && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mcp-spike /mcp-spike
# A fresh named volume inherits this ownership, so nonroot can write generations.json.
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV MCP_DATA_DIR=/data
USER nonroot
ENTRYPOINT ["/mcp-spike"]
