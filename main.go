package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const serviceName = "pwgen-router"

// Retry policy for backend calls. Any non-2xx response (or transport error) is
// retried with exponential backoff starting at initialBackoff and doubling each
// attempt, up to maxRetries additional attempts.
const (
	maxRetries     = 4
	initialBackoff = 500 * time.Millisecond
)

// version is the build version, injected at link time via
//
//	-ldflags "-X main.version=<tag>"
//
// in the build pipeline. Defaults to "dev" for local builds.
var version = "dev"

// backend represents one of the four downstream services this router fans out to.
type backend struct {
	name string // logical name, e.g. "char"
	url  string // target URL from the corresponding env var
}

// router holds the runtime dependencies for the HTTP handler.
type router struct {
	backends   []backend
	httpClient *http.Client
	tracer     trace.Tracer

	// metrics
	reqTotal      *prometheus.CounterVec
	backendTotal  *prometheus.CounterVec
	reqDuration   *prometheus.HistogramVec
	sleepInjected prometheus.Counter
}

func main() {
	ctx := context.Background()

	// --- Configuration via environment ---------------------------------------
	backends, err := loadBackends()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	listenAddr := getenv("LISTEN_ADDR", ":8080")

	// --- OpenTelemetry tracing ------------------------------------------------
	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		log.Fatalf("failed to initialize tracing: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			log.Printf("error shutting down tracer provider: %v", err)
		}
	}()

	// --- Prometheus metrics ---------------------------------------------------
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	r := newRouter(backends, reg)

	// --- HTTP server ----------------------------------------------------------
	engine := gin.New()
	engine.Use(gin.Recovery())
	// otelgin extracts incoming trace context and starts a server span per request.
	// Skip the /healthz probe endpoint so liveness/readiness checks don't flood traces.
	engine.Use(otelgin.Middleware(serviceName,
		otelgin.WithGinFilter(func(c *gin.Context) bool {
			return c.Request.URL.Path != "/healthz"
		}),
	))

	engine.GET("/", r.handle)
	engine.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	// Prometheus exporter endpoint.
	engine.GET("/metrics", gin.WrapH(promhttp.HandlerFor(reg, promhttp.HandlerOpts{})))

	srv := &http.Server{
		Addr:    listenAddr,
		Handler: engine,
	}

	go func() {
		log.Printf("%s listening on %s", serviceName, listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	// --- Graceful shutdown ----------------------------------------------------
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("shutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown error: %v", err)
	}
}

// loadBackends reads the four required service env vars and returns the
// configured backends. It errors if any are missing.
func loadBackends() ([]backend, error) {
	specs := []struct {
		name   string
		envVar string
	}{
		{"char", "CHAR_SERVICE"},
		{"symbol", "SYMBOL_SERVICE"},
		{"uppercase", "UPPERCASE_SERVICE"},
		{"number", "NUMBER_SERVICE"},
	}

	backends := make([]backend, 0, len(specs))
	var missing []string
	for _, s := range specs {
		v := os.Getenv(s.envVar)
		if v == "" {
			missing = append(missing, s.envVar)
			continue
		}
		backends = append(backends, backend{name: s.name, url: v})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variable(s): %v", missing)
	}
	return backends, nil
}

// newRouter builds a router with its HTTP client and registered metrics.
func newRouter(backends []backend, reg prometheus.Registerer) *router {
	reqTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pwgen_router_requests_total",
		Help: "Total number of requests handled by the router endpoint.",
	}, []string{"code"})

	backendTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "pwgen_router_backend_requests_total",
		Help: "Total number of requests routed to each backend, labeled by outcome.",
	}, []string{"backend", "outcome"})

	reqDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "pwgen_router_request_duration_seconds",
		Help:    "Duration of router requests in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"backend"})

	sleepInjected := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pwgen_router_injected_sleeps_total",
		Help: "Total number of requests into which artificial latency was injected.",
	})

	reg.MustRegister(reqTotal, backendTotal, reqDuration, sleepInjected)

	// otelhttp transport propagates the active trace context (W3C traceparent,
	// baggage, etc.) into the outbound request headers automatically.
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	return &router{
		backends:      backends,
		httpClient:    httpClient,
		tracer:        otel.Tracer(serviceName),
		reqTotal:      reqTotal,
		backendTotal:  backendTotal,
		reqDuration:   reqDuration,
		sleepInjected: sleepInjected,
	}
}

// handle is the / endpoint: it randomly picks a backend, forwards the request,
// and returns the backend's response. About 1 in 100 requests get extra latency.
func (r *router) handle(c *gin.Context) {
	start := time.Now()

	// Use the request-scoped context so spans and headers chain off the
	// server span created by the otelgin middleware.
	ctx := c.Request.Context()

	// Inject artificial latency sparingly (~1%).
	if rand.Intn(100) == 0 {
		sleep := time.Duration(5+rand.Intn(1996)) * time.Millisecond // [5ms, 2000ms]
		r.injectSleep(ctx, sleep)
	}

	be := r.backends[rand.Intn(len(r.backends))]

	ctx, span := r.tracer.Start(ctx, "route-to-backend",
		trace.WithAttributes(
			attribute.String("backend.name", be.name),
			attribute.String("backend.url", be.url),
		),
	)
	defer span.End()

	status, body, err := r.forward(ctx, be)
	r.reqDuration.WithLabelValues(be.name).Observe(time.Since(start).Seconds())

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		r.backendTotal.WithLabelValues(be.name, "error").Inc()
		r.reqTotal.WithLabelValues("502").Inc()
		c.JSON(http.StatusBadGateway, gin.H{
			"error":   "backend request failed",
			"backend": be.name,
			"detail":  err.Error(),
		})
		return
	}

	span.SetAttributes(attribute.Int("backend.status_code", status))
	r.reqTotal.WithLabelValues(fmt.Sprintf("%d", status)).Inc()

	// A non-2xx response is a backend fault: surface it on the span so the
	// error bubbles up through the distributed trace, and label the metric
	// accordingly.
	if status < 200 || status >= 300 {
		err := fmt.Errorf("backend %s returned non-2xx status %d", be.name, status)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		r.backendTotal.WithLabelValues(be.name, "error").Inc()
	} else {
		r.backendTotal.WithLabelValues(be.name, "success").Inc()
	}

	// Mirror the backend's status and body to the caller.
	c.Data(status, "application/octet-stream", body)
}

// injectSleep records and performs an artificial delay, wrapped in its own span.
func (r *router) injectSleep(ctx context.Context, d time.Duration) {
	_, span := r.tracer.Start(ctx, "injected-sleep",
		trace.WithAttributes(attribute.Int64("sleep.ms", d.Milliseconds())),
	)
	defer span.End()
	r.sleepInjected.Inc()
	time.Sleep(d)
}

// forward issues the GET to the chosen backend, carrying trace headers. Any
// non-2xx response (or transport-level error) is retried with exponential
// backoff starting at initialBackoff, up to maxRetries additional attempts.
// The status and body from the final attempt are returned.
func (r *router) forward(ctx context.Context, be backend) (int, []byte, error) {
	backoff := initialBackoff

	var (
		status int
		body   []byte
		err    error
	)

	for attempt := 0; ; attempt++ {
		status, body, err = r.attempt(ctx, be)

		// Success on a 2xx response; return immediately.
		if err == nil && status >= 200 && status < 300 {
			return status, body, nil
		}

		// Out of retries: return whatever the last attempt produced.
		if attempt >= maxRetries {
			return status, body, err
		}

		// Wait out the backoff interval, respecting context cancellation, then
		// double it for the next attempt.
		select {
		case <-ctx.Done():
			return status, body, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// attempt performs a single GET to the backend, carrying trace headers, and
// returns the response status and body (or a transport/read error).
func (r *router) attempt(ctx context.Context, be backend) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, be.url, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("calling %s: %w", be.name, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("reading response from %s: %w", be.name, err)
	}
	return resp.StatusCode, body, nil
}

// initTracing configures the global tracer provider with an OTLP/gRPC exporter.
// The endpoint and other parameters are taken from the standard OTEL_* env vars
// (e.g. OTEL_EXPORTER_OTLP_ENDPOINT). Returns a shutdown function.
func initTracing(ctx context.Context) (func(context.Context) error, error) {
	var opts []otlptracegrpc.Option
	// Authenticate to the collector with a bearer token sourced from the
	// OTEL_EXPORTER_OTLP_TOKEN env var (populated from the "token" key of the
	// otel-bearer-token secret).
	if token := os.Getenv("OTEL_EXPORTER_OTLP_TOKEN"); token != "" {
		opts = append(opts, otlptracegrpc.WithHeaders(map[string]string{
			"Authorization": "Bearer " + token,
		}))
	}

	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating OTLP trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("creating resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	// W3C trace context + baggage propagation, so outbound headers are standard.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
