package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTraceContextAndSpanHierarchy(t *testing.T) {
	var exported atomic.Int32
	otlpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			exported.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer otlpSrv.Close()

	p := NewProvider(Config{
		ServiceName:    "infra-guy-test",
		ServiceVersion: "0.1.32",
		Environment:    "test",
		OTLPEndpoint:   otlpSrv.URL,
		MetricsEnabled: true,
	})

	incomingTP := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, ok := ParseTraceParent(incomingTP)
	if !ok || sc.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID != "00f067aa0ba902b7" || !sc.Sampled {
		t.Fatalf("ParseTraceParent(%q) = (%+v, %v)", incomingTP, sc, ok)
	}

	ctx := ContextWithSpanContext(context.Background(), sc)
	ctx, parentSpan := p.StartSpan(ctx, "worker.run_job", Attr("run.id", "run-1"))
	_, childSpan := p.StartSpan(ctx, "terraform.plan", Attr("project.id", "proj-1"))
	childSpan.End()
	parentSpan.End()

	spans := p.RecentSpans()
	if len(spans) != 2 {
		t.Fatalf("RecentSpans len = %d, want 2", len(spans))
	}
	if spans[0].TraceID != sc.TraceID || spans[0].ParentSpanID != parentSpan.SpanContext().SpanID {
		t.Fatalf("child span linkage mismatch: %+v", spans[0])
	}
	if spans[1].ParentSpanID != sc.SpanID {
		t.Fatalf("parent span linkage mismatch: %+v", spans[1])
	}

	deadline := time.Now().Add(2 * time.Second)
	for exported.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if exported.Load() < 2 {
		t.Fatalf("expected 2 OTLP exported spans, got %d", exported.Load())
	}
}

func TestMiddlewareAndMetricsExposition(t *testing.T) {
	p := NewProvider(Config{
		ServiceName:    "infra-guy",
		ServiceVersion: "0.1.32",
		Environment:    "production",
		MetricsEnabled: true,
	})
	p.RecordRunStatus("ten-1", "proj-1", "core-vpc", "plan", "succeeded")
	p.RecordRunQueueWait("ten-1", "proj-1", "core-vpc", "plan", 1500*time.Millisecond)
	p.RecordRunDuration("ten-1", "proj-1", "core-vpc", "plan", "succeeded", "plan", 12*time.Second)
	p.RecordPolicyEvaluation("ten-1", "proj-1", "core-vpc", "mandatory", "pass", 15*time.Millisecond)
	p.RecordApprovalWait("ten-1", "proj-1", "core-vpc", 45*time.Second)
	p.SetGauge("infraguy_workspace_drifted", map[string]string{
		"tenant_id":    "ten-1",
		"project_id":   "proj-1",
		"project_name": "core-vpc",
		"drift_status": "drifted",
	}, 1)

	mux := http.NewServeMux()
	mux.Handle("/metrics", p.MetricsHandler())
	mux.HandleFunc("/projects/proj-123", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := p.Middleware(mux)

	req := httptest.NewRequest(http.MethodGet, "/projects/proj-123", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /projects/proj-123 status = %d", rec.Code)
	}
	if rec.Header().Get("Traceparent") == "" {
		t.Fatal("expected Traceparent header on HTTP response")
	}

	mReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	mRec := httptest.NewRecorder()
	h.ServeHTTP(mRec, mReq)
	body, _ := io.ReadAll(mRec.Result().Body)
	out := string(body)

	for _, want := range []string{
		`infraguy_runs_total{operation="plan",project_id="proj-1",project_name="core-vpc",status="succeeded",tenant_id="ten-1"} 1`,
		`infraguy_run_queue_wait_seconds_count{operation="plan",project_id="proj-1",project_name="core-vpc",tenant_id="ten-1"} 1`,
		`infraguy_run_duration_seconds_count{operation="plan",phase="plan",project_id="proj-1",project_name="core-vpc",status="succeeded",tenant_id="ten-1"} 1`,
		`infraguy_policy_evaluations_total{outcome="pass",project_id="proj-1",project_name="core-vpc",severity="mandatory",tenant_id="ten-1"} 1`,
		`infraguy_approval_wait_seconds_count{project_id="proj-1",project_name="core-vpc",tenant_id="ten-1"} 1`,
		`infraguy_workspace_drifted{drift_status="drifted",project_id="proj-1",project_name="core-vpc",tenant_id="ten-1"} 1`,
		`http_server_requests_total{http_request_method="GET",http_response_status_code="200",http_route="/projects/{id}"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing expected metric line %q in:\n%s", want, out)
		}
	}
}

func TestGrafanaDashboardValidAndAlignedWithMetrics(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "grafana", "dashboards", "infra-guy-overview.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var dash struct {
		UID        string `json:"uid"`
		Title      string `json:"title"`
		Templating struct {
			List []struct {
				Name string `json:"name"`
			} `json:"list"`
		} `json:"templating"`
		Panels []struct {
			Title   string `json:"title"`
			Type    string `json:"type"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dash); err != nil {
		t.Fatalf("Unmarshal dashboard JSON: %v", err)
	}
	if dash.UID != "infra-guy-overview" {
		t.Fatalf("unexpected dashboard uid: %q", dash.UID)
	}
	vars := map[string]bool{}
	for _, v := range dash.Templating.List {
		vars[v.Name] = true
	}
	for _, reqVar := range []string{"datasource", "tenant", "project", "operation"} {
		if !vars[reqVar] {
			t.Errorf("missing template variable $%s in Grafana dashboard", reqVar)
		}
	}
	rows := 0
	for _, p := range dash.Panels {
		if p.Type == "row" {
			rows++
		}
	}
	if rows != 4 {
		t.Fatalf("expected 4 rows in Grafana dashboard, got %d", rows)
	}

	p := NewProvider(Config{ServiceName: "infra-guy", MetricsEnabled: true})
	mRec := httptest.NewRecorder()
	p.MetricsHandler().ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	metricsOut := mRec.Body.String()

	for _, metricName := range []string{
		"infraguy_runs_total",
		"infraguy_runs_in_flight",
		"infraguy_workspace_drifted",
		"infraguy_run_duration_seconds",
		"infraguy_run_queue_wait_seconds",
		"infraguy_approval_wait_seconds",
		"infraguy_policy_evaluations_total",
		"infraguy_policy_evaluation_duration_seconds",
		"infraguy_state_locks_active",
		"http_server_requests_total",
		"http_server_request_duration_seconds",
		"db_client_connections_usage",
		"db_client_connections_max",
		"go_memstats_alloc_bytes",
		"go_goroutines",
	} {
		if !strings.Contains(metricsOut, "# TYPE "+metricName+" ") {
			t.Errorf("metric %q referenced by dashboard is not registered in /metrics output", metricName)
		}
	}
}
