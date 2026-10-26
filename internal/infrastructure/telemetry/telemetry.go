package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Attribute represents a key-value span or metric attribute.
type Attribute struct {
	Key   string
	Value string
}

func Attr(key, value string) Attribute {
	return Attribute{Key: key, Value: value}
}

func AttrInt(key string, value int) Attribute {
	return Attribute{Key: key, Value: strconv.Itoa(value)}
}

// Config configures the OpenTelemetry provider.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	OTLPEndpoint   string
	TracesExporter string
	MetricsEnabled bool
}

// ConfigFromEnv reads standard OpenTelemetry environment variables.
func ConfigFromEnv(getenv func(string) string) Config {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	svc := strings.TrimSpace(getenv("OTEL_SERVICE_NAME"))
	if svc == "" {
		svc = "infra-guy"
	}
	ver := strings.TrimSpace(getenv("SERVICE_VERSION"))
	if ver == "" {
		ver = "0.1.32"
	}
	env := strings.TrimSpace(getenv("DEPLOYMENT_ENVIRONMENT"))
	if env == "" {
		for _, kv := range strings.Split(getenv("OTEL_RESOURCE_ATTRIBUTES"), ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
			if ok && strings.TrimSpace(k) == "deployment.environment" && strings.TrimSpace(v) != "" {
				env = strings.TrimSpace(v)
			}
		}
	}
	if env == "" {
		env = "production"
	}
	metricsEnabled := true
	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_METRICS_EXPORTER")), "none") {
		metricsEnabled = false
	}
	return Config{
		ServiceName:    svc,
		ServiceVersion: ver,
		Environment:    env,
		OTLPEndpoint:   strings.TrimRight(strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")), "/"),
		TracesExporter: strings.TrimSpace(getenv("OTEL_TRACES_EXPORTER")),
		MetricsEnabled: metricsEnabled,
	}
}

var (
	defaultMu       sync.RWMutex
	defaultProvider = NewProvider(Config{
		ServiceName:    "infra-guy",
		ServiceVersion: "0.1.32",
		Environment:    "production",
		MetricsEnabled: true,
	})
)

// Default returns the process-wide telemetry Provider.
func Default() *Provider {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultProvider
}

// SetDefault replaces the process-wide telemetry Provider.
func SetDefault(p *Provider) {
	if p == nil {
		return
	}
	defaultMu.Lock()
	defaultProvider = p
	defaultMu.Unlock()
}

// Default histogram bucket boundaries.
var (
	HTTPDurationBuckets   = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	QueueWaitBuckets      = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	RunDurationBuckets    = []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800}
	PolicyEvalBuckets     = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5}
	ApprovalWaitBuckets   = []float64{1, 5, 15, 30, 60, 300, 900, 1800, 3600, 14400, 86400}
)

// Provider manages OpenTelemetry metrics and distributed traces.
type Provider struct {
	cfg        Config
	httpClient *http.Client

	mu              sync.RWMutex
	counters        map[string]*counterMetric
	gauges          map[string]*gaugeMetric
	histograms      map[string]*histogramMetric
	scrapeCallbacks []func(context.Context, *Provider)

	spanMu      sync.Mutex
	recentSpans []SpanData
	spansTotal  atomic.Int64
}

func NewProvider(cfg Config) *Provider {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "infra-guy"
	}
	if cfg.ServiceVersion == "" {
		cfg.ServiceVersion = "0.1.32"
	}
	if cfg.Environment == "" {
		cfg.Environment = "production"
	}
	p := &Provider{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		counters:   make(map[string]*counterMetric),
		gauges:     make(map[string]*gaugeMetric),
		histograms: make(map[string]*histogramMetric),
	}
	p.registerBuiltinMetrics()
	p.SetGauge("infraguy_build_info", map[string]string{
		"service_name": cfg.ServiceName,
		"version":      cfg.ServiceVersion,
		"environment":  cfg.Environment,
	}, 1)
	return p
}

func (p *Provider) registerBuiltinMetrics() {
	p.ensureCounter("infraguy_runs_total", "Total number of IaC runs by operation and terminal/lifecycle status.")
	p.ensureGauge("infraguy_runs_in_flight", "Number of currently active/running IaC runs.")
	p.ensureHistogram("infraguy_run_queue_wait_seconds", "Time a run spent waiting in the queue before a worker claimed it.", QueueWaitBuckets)
	p.ensureHistogram("infraguy_run_duration_seconds", "Execution duration of IaC runs and individual phases in seconds.", RunDurationBuckets)
	p.ensureCounter("infraguy_policy_evaluations_total", "Total number of OPA policy evaluations by severity and outcome.")
	p.ensureHistogram("infraguy_policy_evaluation_duration_seconds", "Duration of OPA policy evaluations in seconds.", PolicyEvalBuckets)
	p.ensureHistogram("infraguy_approval_wait_seconds", "Time runs spent awaiting manual approval before being approved.", ApprovalWaitBuckets)
	p.ensureGauge("infraguy_workspace_drifted", "Whether a workspace is currently drifted (1 = drifted, 0 = synced/unknown).")
	p.ensureGauge("infraguy_state_locks_active", "Whether a workspace state is currently locked (1 = locked, 0 = unlocked).")
	p.ensureCounter("http_server_requests_total", "Total number of HTTP requests handled by the server.")
	p.ensureHistogram("http_server_request_duration_seconds", "HTTP server request duration in seconds.", HTTPDurationBuckets)
	p.ensureGauge("db_client_connections_usage", "PostgreSQL connection pool usage by state (acquired, idle, constructing).")
	p.ensureGauge("db_client_connections_max", "Maximum configured PostgreSQL pool connections.")
	p.ensureGauge("infraguy_build_info", "Build and resource metadata for the infra-guy service.")
	p.ensureCounter("infraguy_traces_spans_total", "Total number of OpenTelemetry trace spans recorded.")
}

// AddScrapeCallback registers a function invoked prior to rendering /metrics so
// live gauges (e.g., DB pool stats, workspace drift, state locks) are up to date.
func (p *Provider) AddScrapeCallback(fn func(ctx context.Context, p *Provider)) {
	if p == nil || fn == nil {
		return
	}
	p.mu.Lock()
	p.scrapeCallbacks = append(p.scrapeCallbacks, fn)
	p.mu.Unlock()
}

// Shutdown flushes any pending telemetry state on graceful shutdown.
func (p *Provider) Shutdown(_ context.Context) error {
	return nil
}

// IncCounter increments a counter metric by delta.
func (p *Provider) IncCounter(name string, labels map[string]string, delta float64) {
	if p == nil || delta < 0 {
		return
	}
	p.mu.Lock()
	cm := p.ensureCounter(name, name)
	cm.add(labels, delta)
	p.mu.Unlock()
}

// SetGauge sets a gauge series to val.
func (p *Provider) SetGauge(name string, labels map[string]string, val float64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	gm := p.ensureGauge(name, name)
	gm.set(labels, val)
	p.mu.Unlock()
}

// ResetGauge clears all series of a gauge metric prior to refreshing from a snapshot.
func (p *Provider) ResetGauge(name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if gm, ok := p.gauges[name]; ok {
		gm.series = make(map[string]*gaugeSeries)
	}
	p.mu.Unlock()
}

// ObserveHistogram records an observation in a histogram metric.
func (p *Provider) ObserveHistogram(name string, labels map[string]string, val float64) {
	if p == nil || math.IsNaN(val) || val < 0 {
		return
	}
	p.mu.Lock()
	hm := p.ensureHistogram(name, name, RunDurationBuckets)
	hm.observe(labels, val)
	p.mu.Unlock()
}

// High-level domain helpers for infra-guy instrumentation:

func (p *Provider) RecordRunStatus(tenantID, projectID, projectName, operation, status string) {
	p.IncCounter("infraguy_runs_total", map[string]string{
		"tenant_id":    tenantID,
		"project_id":   projectID,
		"project_name": projectName,
		"operation":    operation,
		"status":       status,
	}, 1)
}

func (p *Provider) RecordRunQueueWait(tenantID, projectID, projectName, operation string, d time.Duration) {
	p.ObserveHistogram("infraguy_run_queue_wait_seconds", map[string]string{
		"tenant_id":    tenantID,
		"project_id":   projectID,
		"project_name": projectName,
		"operation":    operation,
	}, d.Seconds())
}

func (p *Provider) RecordRunDuration(tenantID, projectID, projectName, operation, status, phase string, d time.Duration) {
	p.ObserveHistogram("infraguy_run_duration_seconds", map[string]string{
		"tenant_id":    tenantID,
		"project_id":   projectID,
		"project_name": projectName,
		"operation":    operation,
		"status":       status,
		"phase":        phase,
	}, d.Seconds())
}

func (p *Provider) RecordPolicyEvaluation(tenantID, projectID, projectName, severity, outcome string, d time.Duration) {
	if severity == "" {
		severity = "mandatory"
	}
	p.IncCounter("infraguy_policy_evaluations_total", map[string]string{
		"tenant_id":    tenantID,
		"project_id":   projectID,
		"project_name": projectName,
		"severity":     severity,
		"outcome":      outcome,
	}, 1)
	p.ObserveHistogram("infraguy_policy_evaluation_duration_seconds", map[string]string{
		"tenant_id": tenantID,
		"outcome":   outcome,
	}, d.Seconds())
}

func (p *Provider) RecordApprovalWait(tenantID, projectID, projectName string, d time.Duration) {
	p.ObserveHistogram("infraguy_approval_wait_seconds", map[string]string{
		"tenant_id":    tenantID,
		"project_id":   projectID,
		"project_name": projectName,
	}, d.Seconds())
}

func (p *Provider) RecordHTTPRequest(method, route string, status int, d time.Duration) {
	labels := map[string]string{
		"http_request_method":       method,
		"http_route":                route,
		"http_response_status_code": strconv.Itoa(status),
	}
	p.IncCounter("http_server_requests_total", labels, 1)
	p.ObserveHistogram("http_server_request_duration_seconds", labels, d.Seconds())
}

// --- Distributed Tracing (W3C TraceContext + OTLP/HTTP) ---

type traceContextKey struct{}

type SpanContext struct {
	TraceID string
	SpanID  string
	Sampled bool
}

func (sc SpanContext) IsValid() bool {
	return len(sc.TraceID) == 32 && len(sc.SpanID) == 16
}

func (sc SpanContext) TraceParent() string {
	if !sc.IsValid() {
		return ""
	}
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceID + "-" + sc.SpanID + "-" + flags
}

// ParseTraceParent parses a W3C traceparent header ("00-<32hex>-<16hex>-<2hex>").
func ParseTraceParent(raw string) (SpanContext, bool) {
	parts := strings.Split(strings.TrimSpace(raw), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return SpanContext{}, false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil || parts[1] == strings.Repeat("0", 32) {
		return SpanContext{}, false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil || parts[2] == strings.Repeat("0", 16) {
		return SpanContext{}, false
	}
	flagByte, err := hex.DecodeString(parts[3])
	if err != nil || len(flagByte) != 1 {
		return SpanContext{}, false
	}
	return SpanContext{
		TraceID: strings.ToLower(parts[1]),
		SpanID:  strings.ToLower(parts[2]),
		Sampled: (flagByte[0] & 0x01) == 0x01,
	}, true
}

func SpanContextFromContext(ctx context.Context) (SpanContext, bool) {
	if ctx == nil {
		return SpanContext{}, false
	}
	sc, ok := ctx.Value(traceContextKey{}).(SpanContext)
	return sc, ok && sc.IsValid()
}

func ContextWithSpanContext(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, traceContextKey{}, sc)
}

type SpanData struct {
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id,omitempty"`
	Name         string            `json:"name"`
	StartTime    time.Time         `json:"start_time"`
	EndTime      time.Time         `json:"end_time"`
	DurationMs   float64           `json:"duration_ms"`
	Status       string            `json:"status"`
	ErrorMessage string            `json:"error_message,omitempty"`
	Attributes   map[string]string `json:"attributes,omitempty"`
}

type Span struct {
	provider *Provider
	data     SpanData
	sc       SpanContext
	ended    bool
}

func (s *Span) SpanContext() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return s.sc
}

func (s *Span) SetAttribute(key, value string) {
	if s == nil || s.ended || key == "" {
		return
	}
	if s.data.Attributes == nil {
		s.data.Attributes = make(map[string]string)
	}
	s.data.Attributes[key] = value
}

func (s *Span) RecordError(err error) {
	if s == nil || s.ended || err == nil {
		return
	}
	s.data.Status = "ERROR"
	s.data.ErrorMessage = err.Error()
}

func (s *Span) End() {
	if s == nil || s.ended {
		return
	}
	s.ended = true
	s.data.EndTime = time.Now().UTC()
	s.data.DurationMs = float64(s.data.EndTime.Sub(s.data.StartTime).Microseconds()) / 1000.0
	if s.data.Status == "" {
		s.data.Status = "OK"
	}
	if s.provider != nil {
		s.provider.recordSpan(s.data)
	}
}

// StartSpan starts a new OpenTelemetry span on Default().
func StartSpan(ctx context.Context, name string, attrs ...Attribute) (context.Context, *Span) {
	return Default().StartSpan(ctx, name, attrs...)
}

func (p *Provider) StartSpan(ctx context.Context, name string, attrs ...Attribute) (context.Context, *Span) {
	parent, hasParent := SpanContextFromContext(ctx)
	traceID := parent.TraceID
	if traceID == "" {
		traceID = randomHex(16)
	}
	spanID := randomHex(8)
	sc := SpanContext{
		TraceID: traceID,
		SpanID:  spanID,
		Sampled: true,
	}
	attrMap := make(map[string]string, len(attrs))
	for _, a := range attrs {
		if a.Key != "" {
			attrMap[a.Key] = a.Value
		}
	}
	sp := &Span{
		provider: p,
		sc:       sc,
		data: SpanData{
			TraceID:      traceID,
			SpanID:       spanID,
			ParentSpanID: parent.SpanID,
			Name:         name,
			StartTime:    time.Now().UTC(),
			Status:       "OK",
			Attributes:   attrMap,
		},
	}
	if !hasParent {
		sp.data.ParentSpanID = ""
	}
	return ContextWithSpanContext(ctx, sc), sp
}

func (p *Provider) recordSpan(sd SpanData) {
	p.spansTotal.Add(1)
	p.IncCounter("infraguy_traces_spans_total", map[string]string{
		"span_name": sd.Name,
		"status":    sd.Status,
	}, 1)

	p.spanMu.Lock()
	if len(p.recentSpans) >= 256 {
		p.recentSpans = append(p.recentSpans[1:], sd)
	} else {
		p.recentSpans = append(p.recentSpans, sd)
	}
	p.spanMu.Unlock()

	if p.cfg.OTLPEndpoint != "" && !strings.EqualFold(p.cfg.TracesExporter, "none") {
		go p.exportSpanOTLP(sd)
	}
}

// RecentSpans returns a snapshot of recently recorded spans (useful for testing and diagnostics).
func (p *Provider) RecentSpans() []SpanData {
	if p == nil {
		return nil
	}
	p.spanMu.Lock()
	defer p.spanMu.Unlock()
	out := make([]SpanData, len(p.recentSpans))
	copy(out, p.recentSpans)
	return out
}

func (p *Provider) exportSpanOTLP(sd SpanData) {
	attrs := []map[string]any{}
	for k, v := range sd.Attributes {
		attrs = append(attrs, map[string]any{
			"key":   k,
			"value": map[string]any{"stringValue": v},
		})
	}
	statusCode := 1 // STATUS_CODE_OK
	if sd.Status == "ERROR" {
		statusCode = 2 // STATUS_CODE_ERROR
	}
	payload := map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{"key": "service.name", "value": map[string]any{"stringValue": p.cfg.ServiceName}},
						{"key": "service.version", "value": map[string]any{"stringValue": p.cfg.ServiceVersion}},
						{"key": "deployment.environment", "value": map[string]any{"stringValue": p.cfg.Environment}},
					},
				},
				"scopeSpans": []map[string]any{
					{
						"scope": map[string]any{"name": "infra-guy/telemetry"},
						"spans": []map[string]any{
							{
								"traceId":           sd.TraceID,
								"spanId":            sd.SpanID,
								"parentSpanId":      sd.ParentSpanID,
								"name":              sd.Name,
								"kind":              1,
								"startTimeUnixNano": strconv.FormatInt(sd.StartTime.UnixNano(), 10),
								"endTimeUnixNano":   strconv.FormatInt(sd.EndTime.UnixNano(), 10),
								"attributes":        attrs,
								"status": map[string]any{
									"code":    statusCode,
									"message": sd.ErrorMessage,
								},
							},
						},
					},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, p.cfg.OTLPEndpoint+"/v1/traces", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err == nil && resp != nil {
		_ = resp.Body.Close()
	}
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("a", nBytes*2)
	}
	return hex.EncodeToString(b)
}

// --- HTTP Middleware & Prometheus /metrics Handler ---

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// Middleware wraps an http.Handler with W3C TraceContext propagation, span creation,
// and HTTP request metrics (`http_server_requests_total`, `http_server_request_duration_seconds`).
func (p *Provider) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		if incoming, ok := ParseTraceParent(r.Header.Get("Traceparent")); ok {
			ctx = ContextWithSpanContext(ctx, incoming)
		}
		route := NormalizeHTTPRoute(r.URL.Path)
		ctx, span := p.StartSpan(ctx, r.Method+" "+route,
			Attr("http.request.method", r.Method),
			Attr("http.route", route),
			Attr("url.path", r.URL.Path),
		)
		if tp := span.SpanContext().TraceParent(); tp != "" {
			w.Header().Set("Traceparent", tp)
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))
		dur := time.Since(start)

		span.SetAttribute("http.response.status_code", strconv.Itoa(rec.status))
		if rec.status >= 500 {
			span.RecordError(fmt.Errorf("HTTP %d", rec.status))
		}
		span.End()

		p.RecordHTTPRequest(r.Method, route, rec.status, dur)
	})
}

// NormalizeHTTPRoute collapses dynamic IDs in URL paths into low-cardinality route templates.
func NormalizeHTTPRoute(path string) string {
	switch {
	case path == "/" || path == "/login" || path == "/logout" || path == "/metrics" ||
		path == "/.well-known/openid-configuration" || path == "/.well-known/jwks.json":
		return path
	case strings.HasPrefix(path, "/static/"):
		return "/static/*"
	case strings.HasPrefix(path, "/webhooks/"):
		return "/webhooks/github"
	case strings.HasPrefix(path, "/api/v1/state/"):
		return "/api/v1/state/{project_id}"
	case strings.HasPrefix(path, "/api/"):
		return "/api/*"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "/"
	}
	top := parts[0]
	switch top {
	case "projects", "runs", "contexts", "policies", "cloud-integrations", "audit-logs", "finops", "settings", "tenants", "auth":
		if len(parts) == 1 {
			return "/" + top
		}
		if len(parts) == 2 {
			if parts[1] == "new" {
				return "/" + top + "/new"
			}
			return "/" + top + "/{id}"
		}
		return "/" + top + "/{id}/" + parts[2]
	default:
		return "/other"
	}
}

// MetricsHandler returns an http.Handler serving Prometheus text format metrics at /metrics.
func (p *Provider) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.RLock()
		cbs := append([]func(context.Context, *Provider){}, p.scrapeCallbacks...)
		p.mu.RUnlock()

		for _, cb := range cbs {
			cb(r.Context(), p)
		}

		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		p.SetGauge("go_goroutines", nil, float64(runtime.NumGoroutine()))
		p.SetGauge("go_memstats_alloc_bytes", nil, float64(mem.Alloc))
		p.SetGauge("process_resident_memory_bytes", nil, float64(mem.Sys))

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(p.RenderPrometheus()))
	})
}

// RenderPrometheus serializes all registered counters, gauges, and histograms in Prometheus exposition format.
func (p *Provider) RenderPrometheus() string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var sb strings.Builder

	counterNames := make([]string, 0, len(p.counters))
	for name := range p.counters {
		counterNames = append(counterNames, name)
	}
	sort.Strings(counterNames)
	for _, name := range counterNames {
		c := p.counters[name]
		sb.WriteString("# HELP " + c.name + " " + c.help + "\n")
		sb.WriteString("# TYPE " + c.name + " counter\n")
		keys := make([]string, 0, len(c.series))
		for k := range c.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := c.series[k]
			sb.WriteString(fmt.Sprintf("%s%s %g\n", c.name, formatLabels(s.labels), s.val))
		}
	}

	gaugeNames := make([]string, 0, len(p.gauges))
	for name := range p.gauges {
		gaugeNames = append(gaugeNames, name)
	}
	sort.Strings(gaugeNames)
	for _, name := range gaugeNames {
		g := p.gauges[name]
		sb.WriteString("# HELP " + g.name + " " + g.help + "\n")
		sb.WriteString("# TYPE " + g.name + " gauge\n")
		keys := make([]string, 0, len(g.series))
		for k := range g.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := g.series[k]
			sb.WriteString(fmt.Sprintf("%s%s %g\n", g.name, formatLabels(s.labels), s.val))
		}
	}

	histNames := make([]string, 0, len(p.histograms))
	for name := range p.histograms {
		histNames = append(histNames, name)
	}
	sort.Strings(histNames)
	for _, name := range histNames {
		h := p.histograms[name]
		sb.WriteString("# HELP " + h.name + " " + h.help + "\n")
		sb.WriteString("# TYPE " + h.name + " histogram\n")
		keys := make([]string, 0, len(h.series))
		for k := range h.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := h.series[k]
			for i, b := range h.buckets {
				lbls := cloneLabels(s.labels)
				lbls["le"] = strconv.FormatFloat(b, 'f', -1, 64)
				sb.WriteString(fmt.Sprintf("%s_bucket%s %d\n", h.name, formatLabels(lbls), s.counts[i]))
			}
			infLabels := cloneLabels(s.labels)
			infLabels["le"] = "+Inf"
			sb.WriteString(fmt.Sprintf("%s_bucket%s %d\n", h.name, formatLabels(infLabels), s.count))
			sb.WriteString(fmt.Sprintf("%s_sum%s %g\n", h.name, formatLabels(s.labels), s.sum))
			sb.WriteString(fmt.Sprintf("%s_count%s %d\n", h.name, formatLabels(s.labels), s.count))
		}
	}

	return sb.String()
}

// --- Internal Metric Storage ---

type counterSeries struct {
	labels map[string]string
	val    float64
}

type counterMetric struct {
	name   string
	help   string
	series map[string]*counterSeries
}

func (p *Provider) ensureCounter(name, help string) *counterMetric {
	if c, ok := p.counters[name]; ok {
		return c
	}
	c := &counterMetric{name: name, help: help, series: make(map[string]*counterSeries)}
	p.counters[name] = c
	return c
}

func (c *counterMetric) add(labels map[string]string, delta float64) {
	k := labelKey(labels)
	s, ok := c.series[k]
	if !ok {
		s = &counterSeries{labels: cloneLabels(labels)}
		c.series[k] = s
	}
	s.val += delta
}

type gaugeSeries struct {
	labels map[string]string
	val    float64
}

type gaugeMetric struct {
	name   string
	help   string
	series map[string]*gaugeSeries
}

func (p *Provider) ensureGauge(name, help string) *gaugeMetric {
	if g, ok := p.gauges[name]; ok {
		return g
	}
	g := &gaugeMetric{name: name, help: help, series: make(map[string]*gaugeSeries)}
	p.gauges[name] = g
	return g
}

func (g *gaugeMetric) set(labels map[string]string, val float64) {
	k := labelKey(labels)
	s, ok := g.series[k]
	if !ok {
		s = &gaugeSeries{labels: cloneLabels(labels)}
		g.series[k] = s
	}
	s.val = val
}

type histogramSeries struct {
	labels map[string]string
	counts []uint64
	sum    float64
	count  uint64
}

type histogramMetric struct {
	name    string
	help    string
	buckets []float64
	series  map[string]*histogramSeries
}

func (p *Provider) ensureHistogram(name, help string, buckets []float64) *histogramMetric {
	if h, ok := p.histograms[name]; ok {
		return h
	}
	sorted := append([]float64(nil), buckets...)
	sort.Float64s(sorted)
	h := &histogramMetric{
		name:    name,
		help:    help,
		buckets: sorted,
		series:  make(map[string]*histogramSeries),
	}
	p.histograms[name] = h
	return h
}

func (h *histogramMetric) observe(labels map[string]string, val float64) {
	k := labelKey(labels)
	s, ok := h.series[k]
	if !ok {
		s = &histogramSeries{
			labels: cloneLabels(labels),
			counts: make([]uint64, len(h.buckets)),
		}
		h.series[k] = s
	}
	for i, b := range h.buckets {
		if val <= b {
			s.counts[i]++
		}
	}
	s.sum += val
	s.count++
}

func cloneLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('|')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(labels[k])
	}
	return sb.String()
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(labels[k]))
		sb.WriteByte('"')
	}
	sb.WriteByte('}')
	return sb.String()
}

func escapeLabelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}
