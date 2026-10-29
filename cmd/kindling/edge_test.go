package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/siromivel/kindling/internal/spec"
	"go.yaml.in/yaml/v3"
)

const cacheOnly = `
router: {type: mlflow}
connections:
  local: {type: ollama}
models:
  qwen: {connection: local, model: "qwen3:8b"}
endpoints:
  chat: {route: [qwen]}
middleware:
  stages:
    - {type: cache, on_error: fail_open, ttl: 1h, max_entries: 100}
`

func composeServices(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "docker-compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct{ Services map[string]any }
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("docker-compose.yaml isn't valid YAML: %v\n%s", err, data)
	}
	return slices.Sorted(maps.Keys(c.Services))
}

func TestInitCompose(t *testing.T) {
	withPII := strings.Replace(cacheOnly, "  stages:\n", `  stages:
    - {type: pii, on_error: fail_closed, policy: p}
`, 1) + `  pii_policies:
    p: {roles: [user], entities: {EMAIL_ADDRESS: {operator: replace}}}
`
	for name, tc := range map[string]struct {
		spec string
		want string
	}{
		"starter":    {"", "mlflow postgres"},
		"cache only": {cacheOnly, "edge mlflow postgres"},
		"pii":        {withPII, "edge mlflow postgres presidio-analyzer"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.spec != "" {
				os.WriteFile(filepath.Join(dir, "kindling.yaml"), []byte(tc.spec), 0o644)
			}
			if err := runInit([]string{"-dir", dir}); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(composeServices(t, dir), " "); got != tc.want {
				t.Errorf("services = %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("-compose follows kindling.yaml", func(t *testing.T) {
		dir := t.TempDir()
		runInit([]string{"-dir", dir})
		envBefore, _ := os.ReadFile(filepath.Join(dir, ".env"))
		os.WriteFile(filepath.Join(dir, "kindling.yaml"), []byte(withPII), 0o644)
		if err := runInit([]string{"-dir", dir, "-compose"}); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(composeServices(t, dir), " "); got != "edge mlflow postgres presidio-analyzer" {
			t.Errorf("services = %s", got)
		}
		if kept, _ := os.ReadFile(filepath.Join(dir, "kindling.yaml")); string(kept) != withPII {
			t.Error("-compose overwrote kindling.yaml")
		}
		if envAfter, _ := os.ReadFile(filepath.Join(dir, ".env")); string(envAfter) != string(envBefore) {
			t.Error("-compose changed .env")
		}
	})
}

// TestEdgeChains runs requests through newEdge to check that each endpoint
// gets its own chain: the selected policy, and stages disabled by overrides.
func TestEdgeChains(t *testing.T) {
	var mu sync.Mutex
	var seen []string // what the router received, per request
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string }
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		seen = append(seen, req.Messages[0].Content)
		mu.Unlock()
		io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer router.Close()
	presidio := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Text string }
		json.NewDecoder(r.Body).Decode(&req)
		i := strings.Index(req.Text, "Ann Lee")
		if i < 0 {
			io.WriteString(w, "[]")
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{{"entity_type": "PERSON", "start": i, "end": i + 7, "score": 0.85}})
	}))
	defer presidio.Close()

	s, err := spec.Parse([]byte(`
router: {type: mlflow}
connections:
  local: {type: ollama}
models:
  qwen: {connection: local, model: "qwen3:8b"}
endpoints:
  chat: {route: [qwen]}
  local: {route: [qwen]}
  assist: {route: [qwen]}
middleware:
  upstream: `+router.URL+`
  stages:
    - {type: pii, on_error: fail_closed, policy: standard}
    - {type: cache, on_error: fail_open, ttl: 1h, max_entries: 100}
  pii_policies:
    standard: {roles: [user], entities: {PERSON: {operator: keep}}}
    strict: {roles: [user], entities: {PERSON: {operator: replace}}}
  overrides:
    chat: {pii: {policy: strict}, cache: {enabled: false}}
    local: {pii: {enabled: false}}
`), spec.Env{})
	if err != nil {
		t.Fatal(err)
	}
	if ps := s.Validate(); spec.HasErrors(ps) {
		t.Fatal(ps)
	}
	e, err := newEdge(s, edgeOptions{presidioURL: presidio.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	edgeSrv := httptest.NewServer(e.handler)
	defer edgeSrv.Close()

	ask := func(model string) string {
		resp, err := http.Post(edgeSrv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"I'm Ann Lee"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s: HTTP %d %s", model, resp.StatusCode, b)
		}
		return resp.Header.Get("X-Kindling-Cache")
	}
	for _, model := range []string{"chat", "chat", "local", "assist", "hand-made"} {
		ask(model)
	}
	want := []string{
		"I'm <PERSON>", "I'm <PERSON>", // strict, and not cached
		"I'm Ann Lee", // pii off
		"I'm Ann Lee", // standard keeps PERSON
		// hand-made gets the global chain; its key differs from assist's
		// (no route fingerprint), so it misses
		"I'm Ann Lee",
	}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Errorf("router saw %q, want %q", seen, want)
	}
	if h := ask("assist"); h != "hit" {
		t.Errorf("repeat to assist: X-Kindling-Cache = %q, want hit", h)
	}
	if h := ask("chat"); h != "" {
		t.Errorf("chat has cache disabled, but X-Kindling-Cache = %q", h)
	}
}

func TestEdgeNeedsPresidioURL(t *testing.T) {
	s, err := spec.Parse([]byte(`
router: {type: mlflow}
connections:
  local: {type: ollama}
models:
  qwen: {connection: local, model: "qwen3:8b"}
endpoints:
  chat: {route: [qwen]}
middleware:
  stages:
    - {type: pii, on_error: fail_closed, policy: standard}
  pii_policies:
    standard: {roles: [user], entities: {PERSON: {operator: replace}}}
`), spec.Env{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newEdge(s, edgeOptions{}); err == nil || !strings.Contains(err.Error(), "-presidio-url") {
		t.Errorf("newEdge without a Presidio URL: %v", err)
	}
}
