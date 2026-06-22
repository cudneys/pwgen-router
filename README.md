# pwgen-router

A small Gin microservice that exposes a single `/` endpoint and randomly fans
each request out to one of four backend services. It is fully instrumented with
OpenTelemetry tracing (propagating W3C trace headers to the backends) and
exports Prometheus metrics.

## Endpoints

| Path       | Description                                                        |
|------------|--------------------------------------------------------------------|
| `/`        | Randomly routes the request to one of the four backends.           |
| `/metrics` | Prometheus exposition endpoint.                                    |
| `/healthz` | Liveness probe.                                                    |

## Required environment variables

The four backend targets (each a full URL, e.g. `http://char-svc:8080`):

- `CHAR_SERVICE`
- `SYMBOL_SERVICE`
- `UPPERCASE_SERVICE`
- `NUMBER_SERVICE`

The process exits with an error if any are missing.

## Optional environment variables

- `LISTEN_ADDR` — listen address (default `:8080`).
- Standard OpenTelemetry vars are honored, e.g.:
  - `OTEL_EXPORTER_OTLP_ENDPOINT` — OTLP/gRPC collector endpoint.
  - `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_BSP_*`, etc.
- `GIN_MODE=release` — disable Gin debug logging.

## Behavior notes

- **Random routing:** each `/` request picks one of the four backends uniformly
  at random and forwards a `GET` to it, returning the backend's status and body.
- **Trace propagation:** outbound requests use an `otelhttp` transport, so the
  active trace context is injected as `traceparent` (and baggage) headers.
- **Injected latency:** roughly 1 in 100 requests sleeps for a random duration
  between 5ms and 2s, recorded in its own span and counted in the
  `pwgen_router_injected_sleeps_total` metric.

## Run locally

```sh
CHAR_SERVICE=http://localhost:19001 \
SYMBOL_SERVICE=http://localhost:19002 \
UPPERCASE_SERVICE=http://localhost:19003 \
NUMBER_SERVICE=http://localhost:19004 \
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317 \
go run .
```
