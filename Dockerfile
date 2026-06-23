# syntax=docker/dockerfile:1

# --- Build stage -------------------------------------------------------------
FROM golang:1.26-alpine AS build

# CA certificates for outbound HTTPS (OTLP exporter, backends) — copied into
# the scratch image below, which has no certs of its own.
RUN apk add --no-cache ca-certificates

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build version, supplied by the pipeline (derived from the git tag).
ARG VERSION=dev

# Build the static binary.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/pwgen-router .

# --- Runtime stage -----------------------------------------------------------
FROM scratch

WORKDIR /

# CA certificates so TLS verification works for outbound HTTPS calls.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY --from=build /out/pwgen-router /pwgen-router

# Defaults; override CHAR_SERVICE / SYMBOL_SERVICE / UPPERCASE_SERVICE /
# NUMBER_SERVICE and the OTEL_* vars at runtime.
ENV GIN_MODE=release \
    LISTEN_ADDR=:8080

EXPOSE 8080

# Run as a non-root numeric UID (no /etc/passwd on scratch). Matches the
# Deployment's runAsNonRoot security context.
USER 65532:65532

ENTRYPOINT ["/pwgen-router"]
