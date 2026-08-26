# Multi-stage ultra-lightweight Dockerfile for Terminal Relay Server
FROM golang:1.24-alpine AS builder

WORKDIR /app

RUN apk add --no-cache git ca-certificates

# Copy source code
COPY . .

# Build statically linked binary directly
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /app/terminal-relay .

# Minimal production image
FROM alpine:3.21
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app
RUN mkdir -p /app/data

COPY --from=builder /app/terminal-relay /app/terminal-relay

EXPOSE 8080 10000

ENV PORT=10000
ENV DB_PATH=/app/data/relay.db

ENTRYPOINT ["/app/terminal-relay"]
