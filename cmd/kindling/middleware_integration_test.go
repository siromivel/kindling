//go:build integration

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/siromivel/kindling/internal/middleware/cache"
	"github.com/siromivel/kindling/internal/spec"
)

// TestIntegrationMiddleware runs the stack from `kindling init` with a
// middleware section: MLflow, Postgres, Presidio and the edge, built from
// this checkout. Requests go client → edge → MLflow → stub.
func TestIntegrationMiddleware(t *testing.T) {
	ctx := context.Background()
	stub := startStub(t)
	image := buildEdgeImage(t)

	dir := t.TempDir()
	specPath := filepath.Join(dir, "kindling.yaml")
	writeSpec := func(yaml string) {
		t.Helper()
		yaml = strings.ReplaceAll(yaml, "STUB_PORT", fmt.Sprint(stub.port))
		if err := os.WriteFile(specPath, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSpec(middlewareSpec)
	if err := runInit([]string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, ".env")
	mlflowPort, edgePort := freePort(t), freePort(t)
	setEnv(t, envPath, "MLFLOW_PORT", fmt.Sprint(mlflowPort))
	setEnv(t, envPath, "KINDLING_PROXY_PORT", fmt.Sprint(edgePort))
	setEnv(t, envPath, "KINDLING_IMAGE", image)

	stack := &composeStack{t: t, dir: dir, project: "kindling-it-mw-" + randomSuffix()}
	t.Cleanup(stack.down)
	stack.up()
	if err := runPlan(ctx, []string{"-f", specPath}, true); err != nil {
		t.Fatal(err)
	}
	edgeURL := fmt.Sprintf("http://localhost:%d", edgePort)

	t.Run("pii redacts before the router", func(t *testing.T) {
		stub.reset()
		r := edgeChat(t, edgeURL, "assist", "Email me at alice@example.com or call 212-555-0123.", false)
		if r.content != "from model-a" || r.cache != "miss" {
			t.Errorf("answer %q, cache %q", r.content, r.cache)
		}
		want := []string{"Email me at <EMAIL_ADDRESS> or call ***-***-0123."}
		if got := stub.userMessages(); !slices.Equal(got, want) {
			t.Errorf("router received %q, want %q", got, want)
		}

		// The trace in MLflow: pii → cache miss → upstream → cache stored.
		var spans map[string]map[string]any
		var raw string
		eventually(t, 30*time.Second, func() string {
			spans, raw = stack.trace(r.traceID)
			if spans == nil {
				return "trace " + r.traceID + " isn't in MLflow"
			}
			return ""
		})
		for name, outcome := range map[string]string{"pii.request": "redacted", "cache.request": "miss", "cache.response": "stored"} {
			if got := spans[name]["kindling.outcome"]; got != outcome {
				t.Errorf("%s outcome = %v, want %s (spans %v)", name, got, outcome, spans)
			}
		}
		if _, ok := spans["upstream"]; !ok {
			t.Errorf("no upstream span: %v", spans)
		}
		pii := spans["pii.request"]
		for k, v := range map[string]any{
			"kindling.pii.redacted":               2.0,
			"kindling.pii.EMAIL_ADDRESS.count":    1.0,
			"kindling.pii.EMAIL_ADDRESS.operator": "replace",
			"kindling.pii.PHONE_NUMBER.operator":  "mask",
		} {
			if pii[k] != v {
				t.Errorf("pii span %s = %v, want %v", k, pii[k], v)
			}
		}
		for _, secret := range []string{"alice", "212-555", "555-0123"} {
			if strings.Contains(raw, secret) {
				t.Errorf("the trace contains %q", secret)
			}
		}
	})

	t.Run("pii offsets survive unusual text", func(t *testing.T) {
		// Presidio's offsets are Python code points. Characters outside the
		// BMP, ZWJ sequences, combining marks and invalid bytes (sent raw,
		// so the edge's JSON decoding has to deal with them) must not shift
		// what gets redacted.
		stub.reset()
		prefix := "😀 👩‍💻 e" + string(rune(0x301)) + " \xff\xfe "
		body := `{"model":"assist","messages":[{"role":"user","content":"` + prefix + `\ud83d mail ann@example.com or call 212-555-0123."}]}`
		resp, err := http.Post(edgeURL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		fffd := string(utf8.RuneError)
		want := "😀 👩‍💻 e" + string(rune(0x301)) + " " + fffd + fffd + " " + fffd + " mail <EMAIL_ADDRESS> or call ***-***-0123."
		if got := stub.userMessages(); resp.StatusCode != 200 || !slices.Equal(got, []string{want}) {
			t.Errorf("HTTP %d; router received %q, want %q", resp.StatusCode, got, want)
		}
	})

	t.Run("pii and cache share redacted keys", func(t *testing.T) {
		stub.reset()
		first := edgeChat(t, edgeURL, "assist", "My email is bob@example.org. What's my plan?", false)
		second := edgeChat(t, edgeURL, "assist", "My email is carol@example.net. What's my plan?", false)
		if first.cache != "miss" || second.cache != "hit" || len(stub.userMessages()) != 1 {
			t.Errorf("cache %q then %q, router called %d times; want one call and a hit", first.cache, second.cache, len(stub.userMessages()))
		}
	})

	t.Run("cache", func(t *testing.T) {
		stub.reset()
		first := edgeChat(t, edgeURL, "assist", "What is the capital of France?", false)
		second := edgeChat(t, edgeURL, "assist", "What is the capital of France?", false)
		if second.cache != "hit" || second.content != first.content || len(stub.userMessages()) != 1 {
			t.Errorf("second answer %q (%s), router called %d times", second.content, second.cache, len(stub.userMessages()))
		}
	})

	t.Run("streaming", func(t *testing.T) {
		stub.reset()
		first := edgeChat(t, edgeURL, "assist", "Tell me a story.", true)
		if first.content != "from model-a" || first.events < 2 || first.cache != "miss" {
			t.Errorf("stream: %q in %d events, cache %q", first.content, first.events, first.cache)
		}
		replay := edgeChat(t, edgeURL, "assist", "Tell me a story.", true)
		if replay.content != first.content || replay.events < 2 || replay.cache != "hit" {
			t.Errorf("replay: %q in %d events, cache %q", replay.content, replay.events, replay.cache)
		}
		if n := len(stub.userMessages()); n != 1 {
			t.Errorf("router called %d times, want 1", n)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		stub.reset()
		msg := "I'm Alice Johnson, write to alice@example.com."
		edgeChat(t, edgeURL, "local", msg, false)
		edgeChat(t, edgeURL, "chat", msg, false)
		edgeChat(t, edgeURL, "assist", "I'm Alice Johnson.", false)
		want := []string{
			msg, // local: pii disabled
			"I'm <PERSON>, write to <EMAIL_ADDRESS>.", // chat: strict
			"I'm Alice Johnson.",                      // assist: standard keeps PERSON
		}
		if got := stub.userMessages(); !slices.Equal(got, want) {
			t.Errorf("router received:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		stub.reset()
		again := edgeChat(t, edgeURL, "chat", msg, false)
		if n := len(stub.userMessages()); n != 1 || again.cache != "" {
			t.Errorf("chat has cache disabled, but the router got %d requests, cache header %q", n, again.cache)
		}
	})

	t.Run("pii fails closed", func(t *testing.T) {
		if out, err := stack.compose("stop", "presidio-analyzer"); err != nil {
			t.Fatalf("stopping presidio: %v\n%s", err, out)
		}
		defer stack.up()
		stub.reset()
		resp := postChat(t, edgeURL, "assist", "My SSN is 536-22-8726.", false)
		defer resp.Body.Close()
		var out struct {
			Error struct{ Message, Type string }
		}
		json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != 503 || out.Error.Type != "middleware_error" || !strings.Contains(out.Error.Message, "pii") {
			t.Errorf("HTTP %d, %+v", resp.StatusCode, out.Error)
		}
		if n := len(stub.userMessages()); n != 0 {
			t.Errorf("router got %d requests while Presidio was down", n)
		}
	})

	// The rest run edges in-process against the same MLflow, with only a
	// cache stage (Presidio isn't reachable from the host).
	cacheSpec := func(model string) *spec.Spec {
		t.Helper()
		s, err := spec.ParseForEdge([]byte(strings.NewReplacer(
			"STUB_PORT", fmt.Sprint(stub.port), "assist: {route: [a]}", "assist: {route: ["+model+"]}",
		).Replace(cacheOnlySpec)), spec.Env{"MLFLOW_PORT": fmt.Sprint(mlflowPort)})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	startEdge := func(s *spec.Spec, store cache.Store) string {
		t.Helper()
		e, err := newEdge(s, edgeOptions{store: store})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(e.handler)
		t.Cleanup(func() { srv.Close(); e.close() })
		return srv.URL
	}

	t.Run("cache fails open", func(t *testing.T) {
		url := startEdge(cacheSpec("a"), brokenStore{})
		r := edgeChat(t, url, "assist", "Does a broken cache matter?", false)
		if r.content != "from model-a" || r.cache != "" {
			t.Errorf("answer %q, cache header %q", r.content, r.cache)
		}
	})

	t.Run("route change invalidates the cache", func(t *testing.T) {
		// The store outlives the edges, as a shared store (Redis) would, so a
		// miss after the restart is down to the route fingerprint.
		store := cache.NewLRU(100)
		before := startEdge(cacheSpec("a"), store)
		edgeChat(t, before, "assist", "What's 2+2?", false)
		if r := edgeChat(t, before, "assist", "What's 2+2?", false); r.cache != "hit" {
			t.Fatalf("repeat request: cache %q, want hit", r.cache)
		}
		if r := edgeChat(t, startEdge(cacheSpec("a"), store), "assist", "What's 2+2?", false); r.cache != "hit" {
			t.Errorf("restarted with the same route: cache %q, want hit", r.cache)
		}

		writeSpec(strings.Replace(middlewareSpec, "assist: {route: [a]}", "assist: {route: [b]}", 1))
		if err := runPlan(ctx, []string{"-f", specPath}, true); err != nil {
			t.Fatal(err)
		}
		after := startEdge(cacheSpec("b"), store)
		if r := edgeChat(t, after, "assist", "What's 2+2?", false); r.cache != "miss" {
			t.Errorf("after moving assist to model b: cache %q, want miss", r.cache)
		}
		// MLflow workers pick up the new route within their cache TTL.
		eventually(t, 30*time.Second, func() string {
			if r := edgeChat(t, after, "assist", fmt.Sprint("What's 2+2? ", time.Now().UnixNano()), false); r.content != "from model-b" {
				return "assist still answered " + r.content
			}
			return ""
		})
	})

}

// TestIntegrationNoPresidio checks that a stack with only a cache stage has
// no Presidio, and starts.
func TestIntegrationNoPresidio(t *testing.T) {
	image := buildEdgeImage(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kindling.yaml"), []byte(strings.ReplaceAll(cacheOnlySpec, "STUB_PORT", "1")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, ".env")
	edgePort := freePort(t)
	setEnv(t, envPath, "MLFLOW_PORT", fmt.Sprint(freePort(t)))
	setEnv(t, envPath, "KINDLING_PROXY_PORT", fmt.Sprint(edgePort))
	setEnv(t, envPath, "KINDLING_IMAGE", image)

	stack := &composeStack{t: t, dir: dir, project: "kindling-it-np-" + randomSuffix()}
	out, err := stack.compose("config", "--services")
	if err != nil {
		t.Fatalf("compose config: %v\n%s", err, out)
	}
	if services := strings.Fields(string(out)); slices.Contains(services, "presidio-analyzer") || !slices.Contains(services, "edge") {
		t.Fatalf("services = %v, want an edge and no Presidio", services)
	}
	t.Cleanup(stack.down)
	stack.up()
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", edgePort))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("edge healthz: %v %v", resp, err)
	}
}

const middlewareSpec = `
router:
  type: mlflow
  url: http://localhost:${MLFLOW_PORT:-5050}
connections:
  stub: {type: openai-compatible, base_url: "http://host.docker.internal:STUB_PORT/v1", api_key: sk-stub-key-0001}
models:
  a: {connection: stub, model: model-a}
  b: {connection: stub, model: model-b}
endpoints:
  assist: {route: [a]}
  chat: {route: [a]}
  local: {route: [a]}
middleware:
  stages:
    - {type: pii, on_error: fail_closed, policy: standard}
    - {type: cache, on_error: fail_open, mode: exact, ttl: 1h, max_entries: 1000}
  pii_policies:
    standard:
      roles: [user, system]
      entities:
        EMAIL_ADDRESS: {operator: replace}
        # Presidio scores a phone number 0.4 without a context word like
        # "phone" nearby.
        PHONE_NUMBER: {operator: mask, score_threshold: 0.4}
        PERSON: {operator: keep}
    strict:
      roles: [user, system]
      entities:
        EMAIL_ADDRESS: {operator: replace}
        PHONE_NUMBER: {operator: replace}
        PERSON: {operator: replace}
  overrides:
    chat: {pii: {policy: strict}, cache: {enabled: false}}
    local: {pii: {enabled: false}}
`

const cacheOnlySpec = `
router:
  type: mlflow
  url: http://localhost:${MLFLOW_PORT:-5050}
connections:
  stub: {type: openai-compatible, base_url: "http://host.docker.internal:STUB_PORT/v1", api_key: sk-stub-key-0001}
models:
  a: {connection: stub, model: model-a}
  b: {connection: stub, model: model-b}
endpoints:
  assist: {route: [a]}
middleware:
  stages:
    - {type: cache, on_error: fail_open, ttl: 1h, max_entries: 100}
`

type brokenStore struct{}

func (brokenStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("store down")
}
func (brokenStore) Set(context.Context, string, []byte, time.Duration) error {
	return errors.New("store down")
}

// buildEdgeImage builds the edge image from this checkout, once per test run.
// The tag is fixed so reruns replace it rather than pile up.
func buildEdgeImage(t *testing.T) string {
	t.Helper()
	builtOnce.Do(func() {
		builtImage = "kindling-it:latest"
		cmd := exec.Command("docker", "build", "-q", "-t", builtImage, "../..")
		if out, err := cmd.CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("docker build: %v\n%s", err, out)
		}
	})
	if builtErr != nil {
		t.Fatal(builtErr)
	}
	return builtImage
}

var (
	builtOnce  sync.Once
	builtImage string
	builtErr   error
)

type chatResult struct {
	content, cache, traceID string
	events                  int
}

func postChat(t *testing.T, baseURL, model, content string, stream bool) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []map[string]string{{"role": "user", "content": content}},
	})
	resp, err := http.Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// edgeChat sends one request through an edge and returns the answer.
func edgeChat(t *testing.T, baseURL, model, content string, stream bool) chatResult {
	t.Helper()
	resp := postChat(t, baseURL, model, content, stream)
	defer resp.Body.Close()
	r := chatResult{cache: resp.Header.Get(cache.Header), traceID: resp.Header.Get("X-Kindling-Trace-Id")}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: HTTP %d: %s", model, resp.StatusCode, b)
	}
	if !stream {
		var out struct {
			Choices []struct{ Message struct{ Content string } }
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
			t.Fatalf("%s: %v %+v", model, err, out)
		}
		r.content = out.Choices[0].Message.Content
		return r
	}
	sc := bufio.NewScanner(resp.Body)
	var b strings.Builder
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		r.events++
		var c struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		json.Unmarshal([]byte(data), &c)
		for _, ch := range c.Choices {
			b.WriteString(ch.Delta.Content)
		}
	}
	r.content = b.String()
	return r
}

// trace returns the attributes of each span of an edge trace stored in
// MLflow, by span name, and the whole trace as JSON; nil if it isn't there.
func (c *composeStack) trace(traceID string) (map[string]map[string]any, string) {
	c.t.Helper()
	script := fmt.Sprintf(`
import json, mlflow
mlflow.set_tracking_uri("http://localhost:5000")
try:
    t = mlflow.get_trace("tr-%s")
except Exception:
    t = None
if t is None:
    print("null")
else:
    print(json.dumps({"spans": {s.name: s.attributes for s in t.data.spans}, "raw": t.to_json()}))
`, traceID)
	out, err := c.compose("exec", "-T", "mlflow", "python", "-c", script)
	if err != nil {
		c.t.Fatalf("reading trace: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var got *struct {
		Spans map[string]map[string]any
		Raw   string
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		c.t.Fatalf("reading trace: %v\n%s", err, out)
	}
	if got == nil {
		return nil, ""
	}
	return got.Spans, got.Raw
}
