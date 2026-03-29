# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /ynab-autocategorizer .

# Runtime stage
FROM alpine:3.21

RUN apk --no-cache add ca-certificates tzdata
COPY --from=builder /ynab-autocategorizer /usr/local/bin/ynab-autocategorizer

ENTRYPOINT ["ynab-autocategorizer"]
