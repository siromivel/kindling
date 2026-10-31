package mlflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/spec"
)

// TestTraceExporter checks the experiment lookup and the OTLP request against
// a stand-in for MLflow; the integration test checks the real thing.
func TestTraceExporter(t *testing.T) {
	var created, gets int
	var export struct {
		header http.Header
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/2.0/mlflow/experiments/get-by-name":
			gets++
			if created == 0 {
				w.WriteHeader(404)
				json.NewEncoder(w).Encode(map[string]string{"error_code": "RESOURCE_DOES_NOT_EXIST", "message": "nope"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"experiment": map[string]string{"experiment_id": "7"}})
		case "/api/2.0/mlflow/experiments/create":
			created++
			json.NewEncoder(w).Encode(map[string]string{"experiment_id": "7"})
		case "/v1/traces":
			export.header = r.Header.Clone()
			json.NewDecoder(r.Body).Decode(&export.body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := &spec.Spec{Router: spec.Router{Type: "mlflow", URL: srv.URL}}
	b := New(s)
	if got := b.InferenceURL(s); got != srv.URL+"/gateway/mlflow/v1" {
		t.Errorf("InferenceURL = %q", got)
	}
	exp := b.TraceExporter(s)
	trace, root := middleware.NewTrace("chat.completions")
	child := root.Child("pii.request")
	child.SetOutcome("redacted")
	child.Set("kindling.pii.redacted", 2)
	child.End()
	root.End()
	for range 2 {
		if err := exp.Export(context.Background(), trace.Spans()); err != nil {
			t.Fatal(err)
		}
	}
	if created != 1 || gets != 1 {
		t.Errorf("experiment created %d times, looked up %d times; want it resolved once", created, gets)
	}
	if got := export.header.Get("X-Mlflow-Experiment-Id"); got != "7" || export.header.Get("Content-Type") != "application/json" {
		t.Errorf("export headers = %v", export.header)
	}
	spans := export.body["resourceSpans"].([]any)[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
	if len(spans) != 2 {
		t.Fatalf("exported %d spans", len(spans))
	}
	rootSpan, piiSpan := spans[0].(map[string]any), spans[1].(map[string]any)
	if _, ok := rootSpan["parentSpanId"]; ok {
		t.Error("root span must have no parent, or MLflow won't treat the trace as complete")
	}
	if piiSpan["parentSpanId"] != rootSpan["spanId"] || len(piiSpan["traceId"].(string)) != 32 {
		t.Errorf("child span = %v", piiSpan)
	}
}
