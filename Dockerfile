# syntax=docker/dockerfile:1

# --- Build stage -------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Build the static binary.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/pwgen-router .

# --- Runtime stage -----------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=build /out/pwgen-router /pwgen-router

# Defaults; override CHAR_SERVICE / SYMBOL_SERVICE / UPPERCASE_SERVICE /
# NUMBER_SERVICE and the OTEL_* vars at runtime.
ENV GIN_MODE=release \
    LISTEN_ADDR=:8080

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/pwgen-router"]
