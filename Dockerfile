# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e

## Build stage
# Pinned to a digest for supply-chain safety; the readable tag documents the
# version. Renovate keeps both the tag and the digest up to date (see docs/ci.md).
FROM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/screenshotter-org/screenshotter_slack_bridge/internal/version.Version=${VERSION}" \
    -o screenshotter-slack-bridge ./cmd/screenshotter-slack-bridge

## Runtime stage
FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /build/screenshotter-slack-bridge /app/screenshotter-slack-bridge

# Mount your config.toml at /app/config.toml (or provide the tokens via env
# vars). There are no inbound ports to expose: Socket Mode and image uploads
# are both outbound connections.

ENTRYPOINT ["/app/screenshotter-slack-bridge"]
