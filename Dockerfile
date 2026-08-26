# Multi-stage ultra-lightweight Dockerfile for Terminal Relay Server
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy relay module dependencies
COPY relay/go.mod relay/go.sum ./
RUN go mod download

# Copy relay source code
COPY relay/ .

# Build statically linked binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /app/terminal-relay ./cmd/terminal-relay

# Minimal production image
FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/terminal-relay /app/terminal-relay

EXPOSE 8080
VOLUME ["/app/data"]

ENV PORT=8080
ENV DB_PATH=/app/data/relay.db

ENTRYPOINT ["/app/terminal-relay"]
