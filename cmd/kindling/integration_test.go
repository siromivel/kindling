//go:build integration

// Integration tests run the real stack from `kindling init` (MLflow + Postgres
// in Docker) and drive it through the CLI, with a stub OpenAI-compatible server
// standing in for model providers. They need Docker with compose v2:
//
//	go test -tags integration -timeout 20m ./cmd/kindling
//
// Set KINDLING_IT_KEEP=1 to leave the stack running afterwards.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/siromivel/kindling/internal/backend"
	"github.com/siromivel/kindling/internal/mlflow"
	"github.com/siromivel/kindling/internal/spec"
)

func TestIntegrationMLflow(t *testing.T) {
	ctx := context.Background()
	stub := startStub(t)
	deadPort := freePort(t)

	dir := t.TempDir()
	if err := runInit([]string{"-dir", dir}); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, ".env")
	port := freePort(t)
	setEnv(t, envPath, "MLFLOW_PORT", fmt.Sprint(port))
	// Start the way someone with only hosted models would, so the first apply
	// has to turn on private networking.
	setEnv(t, envPath, "MLFLOW_GATEWAY_API_BASE_ALLOW_PRIVATE_IPS", "false")
	setEnv(t, envPath, "MLFLOW_GATEWAY_API_BASE_ALLOWED_SCHEMES", "https")

	stack := &composeStack{t: t, dir: dir, project: "kindling-it-" + randomSuffix()}
	t.Cleanup(stack.down)
	stack.up()

	specPath := filepath.Join(dir, "kindling.yaml")
	baseURL := fmt.Sprintf("http://localhost:%d", port)
	writeSpec := func(yaml string) {
		t.Helper()
		yaml = strings.NewReplacer("STUB_PORT", fmt.Sprint(stub.port), "DEAD_PORT", fmt.Sprint(deadPort)).Replace(yaml)
		if err := os.WriteFile(specPath, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	apply := func(args ...string) error {
		return runPlan(ctx, append([]string{"-f", specPath}, args...), true)
	}
	driftCheck := func() error {
		return runPlan(ctx, []string{"-f", specPath, "-detailed-exitcode"}, false)
	}

	writeSpec(fullSpec)

	t.Run("apply sets up private networking first", func(t *testing.T) {
		if err := apply(); !errors.Is(err, errReported) {
			t.Fatalf("apply = %v, want it to stop after updating .env", err)
		}
		vars, err := spec.ReadDotenv(envPath)
		if err != nil {
			t.Fatal(err)
		}
		if vars["MLFLOW_GATEWAY_API_BASE_ALLOW_PRIVATE_IPS"] != "true" || vars["MLFLOW_GATEWAY_API_BASE_ALLOWED_SCHEMES"] != "http,https" {
			t.Fatalf(".env not updated: %v", vars)
		}
		stack.up() // the restart the CLI tells users to do
	})

	t.Run("apply from scratch", func(t *testing.T) {
		if err := apply(); err != nil {
			t.Fatal(err)
		}
		assertNoChanges(t, specPath)
	})

	t.Run("traffic split", func(t *testing.T) {
		counts := map[string]int{}
		for range 40 {
			counts[chat(t, baseURL, "split")]++
		}
		// 80/20 split: a lopsided-the-wrong-way result over 40 requests has
		// odds around 1 in 100,000.
		if counts["model-a"] <= counts["model-b"] || counts["model-b"] == 0 {
			t.Errorf("responses by model = %v, want mostly model-a and some model-b", counts)
		}
	})

	t.Run("fallback", func(t *testing.T) {
		if got := chat(t, baseURL, "resilient"); got != "model-b" {
			t.Errorf("resilient endpoint answered from %q, want fallback model-b", got)
		}
	})

	t.Run("usage tracking records traces", func(t *testing.T) {
		// Traces are exported in the background after each request. MLflow
		// also logs "Failed to send trace" warnings for gateway traces, but
		// their spans still reach Postgres; that's what users see in the UI.
		eventually(t, 30*time.Second, func() string {
			spans := stack.traceSpans("gateway/split")
			if len(spans) != 40 || slices.Min(spans) < 2 {
				return fmt.Sprintf("span counts for the split endpoint's 40 requests = %v", spans)
			}
			return ""
		})
	})

	t.Run("credentials reach the upstream", func(t *testing.T) {
		stub.reset()
		chat(t, baseURL, "single")
		if got := stub.auths(); !slices.Equal(got, []string{"Bearer sk-stub-key-0001"}) {
			t.Errorf("stub saw Authorization %v", got)
		}
	})

	t.Run("updates", func(t *testing.T) {
		writeSpec(strings.NewReplacer(
			"sk-stub-key-0001", "sk-stub-key-0002",
			"weight: 80", "weight: 30",
			"weight: 20", "weight: 70",
			"model: model-a}", "model: model-a2}",
		).Replace(fullSpec))

		assertChanges(t, specPath, backend.Options{}, []string{
			"~ connection stub (credentials changed)",
			"~ model a (model: model-a → model-a2)",
			"~ endpoint split ([a 80%, b 20%] → [a 30%, b 70%])",
		})
		if err := driftCheck(); !errors.Is(err, errChanges) {
			t.Fatalf("plan -detailed-exitcode = %v, want errChanges", err)
		}
		if err := apply(); err != nil {
			t.Fatal(err)
		}
		assertNoChanges(t, specPath)
		if err := driftCheck(); err != nil {
			t.Fatalf("plan -detailed-exitcode after apply = %v, want nil", err)
		}

		// Workers that didn't handle the update serve cached settings until
		// MLFLOW_SERVER_SECRETS_CACHE_TTL (10s) expires.
		eventually(t, 30*time.Second, func() string {
			stub.reset()
			if got := chat(t, baseURL, "single"); got != "model-a2" {
				return fmt.Sprintf("single endpoint answered from %q, want model-a2", got)
			}
			if got := stub.auths(); !slices.Equal(got, []string{"Bearer sk-stub-key-0002"}) {
				return fmt.Sprintf("stub saw Authorization %v, want the new key", got)
			}
			return ""
		})
	})

	t.Run("prune leaves hand-made objects alone", func(t *testing.T) {
		client := mlflow.NewClient(baseURL)
		if _, err := client.CreateSecret(ctx, mlflow.SecretRequest{
			Name: "handmade", Provider: "openai", Value: map[string]string{"api_key": "sk-handmade-123"},
		}); err != nil {
			t.Fatal(err)
		}

		writeSpec(smallSpec)
		assertChanges(t, specPath, backend.Options{Prune: true}, []string{
			"- endpoint resilient", "- endpoint split",
			"- model b", "- model broken", "- connection dead",
		})
		if err := apply("-prune"); err != nil {
			t.Fatal(err)
		}
		assertNoChanges(t, specPath)

		secrets, err := client.ListSecrets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range secrets {
			names = append(names, s.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"handmade", "stub"}) {
			t.Errorf("connections after prune = %v, want [handmade stub]", names)
		}
	})

	t.Run("refuses to take over hand-made objects", func(t *testing.T) {
		writeSpec(strings.Replace(smallSpec, "\n  stub:", "\n  handmade: {type: openai, api_key: sk-other-456}\n  stub:", 1))
		s, b := load(t, specPath)
		_, err := b.Plan(ctx, s, backend.Options{})
		if err == nil || !strings.Contains(err.Error(), `connection "handmade" already exists in MLflow but wasn't created by kindling`) {
			t.Errorf("plan err = %v", err)
		}
	})
}

const fullSpec = `
router:
  type: mlflow
  url: http://localhost:${MLFLOW_PORT}
connections:
  stub: {type: openai-compatible, base_url: "http://host.docker.internal:STUB_PORT/v1", api_key: sk-stub-key-0001}
  dead: {type: openai-compatible, base_url: "http://host.docker.internal:DEAD_PORT/v1"}
models:
  a: {connection: stub, model: model-a}
  b: {connection: stub, model: model-b}
  broken: {connection: dead, model: nothing}
endpoints:
  single: {route: [a]}
  split:
    route:
      - {model: a, weight: 80}
      - {model: b, weight: 20}
  resilient:
    route: [broken]
    fallback: [b]
`

const smallSpec = `
router:
  type: mlflow
  url: http://localhost:${MLFLOW_PORT}
connections:
  stub: {type: openai-compatible, base_url: "http://host.docker.internal:STUB_PORT/v1", api_key: sk-stub-key-0002}
models:
  a: {connection: stub, model: model-a2}
endpoints:
  single: {route: [a]}
`

func load(t *testing.T, path string) (*spec.Spec, backend.Backend) {
	t.Helper()
	f := specFlags{file: path}
	s, _, b, err := f.load()
	if err != nil {
		t.Fatal(err)
	}
	return s, b
}

func assertChanges(t *testing.T, path string, opts backend.Options, want []string) {
	t.Helper()
	s, b := load(t, path)
	plan, err := b.Plan(context.Background(), s, opts)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range plan.Changes {
		got = append(got, c.String())
	}
	if !slices.Equal(got, want) {
		t.Errorf("plan:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func assertNoChanges(t *testing.T, path string) {
	t.Helper()
	assertChanges(t, path, backend.Options{}, nil)
}

// chat sends one request through the gateway and returns the upstream model
// that answered it.
func chat(t *testing.T, baseURL, endpoint string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    endpoint,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(baseURL+"/gateway/mlflow/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct{ Content string }
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || len(out.Choices) == 0 {
		t.Fatalf("chat %s: HTTP %d, %v, %+v", endpoint, resp.StatusCode, err, out)
	}
	return strings.TrimPrefix(out.Choices[0].Message.Content, "from ")
}

// eventually retries check until it returns "" or the timeout passes, then
// fails with the last message.
func eventually(t *testing.T, timeout time.Duration, check func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msg := check()
		if msg == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %s: %s", timeout, msg)
		}
		time.Sleep(time.Second)
	}
}

// stubServer is a minimal OpenAI-compatible chat API on the host, reachable
// from containers as host.docker.internal. It answers "from <model>", streamed
// in two chunks if asked, and records what it was sent.
type stubServer struct {
	port     int
	mu       sync.Mutex
	seen     []string // Authorization headers
	requests []stubRequest
}

type stubRequest struct {
	Model    string
	Stream   bool
	Messages []struct{ Role, Content string }
}

func startStub(t *testing.T) *stubServer {
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stubServer{port: ln.Addr().(*net.TCPAddr).Port}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req stubRequest
		json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.seen = append(s.seen, r.Header.Get("Authorization"))
		s.requests = append(s.requests, req)
		s.mu.Unlock()
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, delta := range []string{`{"role":"assistant","content":"from "}`, `{"content":"` + req.Model + `"}`} {
				fmt.Fprintf(w, "data: {\"id\":\"stub\",\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":null}]}\n\n", time.Now().Unix(), req.Model, delta)
				w.(http.Flusher).Flush()
			}
			fmt.Fprintf(w, "data: {\"id\":\"stub\",\"object\":\"chat.completion.chunk\",\"created\":%d,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", time.Now().Unix(), req.Model)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "stub", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]string{"role": "assistant", "content": "from " + req.Model},
			}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
		})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return s
}

func (s *stubServer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen, s.requests = nil, nil
}

func (s *stubServer) auths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// userMessages returns the last user message of each request received.
func (s *stubServer) userMessages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.requests {
		for _, m := range slices.Backward(r.Messages) {
			if m.Role == "user" {
				out = append(out, m.Content)
				break
			}
		}
	}
	return out
}

type composeStack struct {
	t            *testing.T
	dir, project string
}

func (c *composeStack) compose(args ...string) ([]byte, error) {
	cmd := exec.Command("docker", append([]string{"compose", "-p", c.project}, args...)...)
	cmd.Dir = c.dir
	return cmd.CombinedOutput()
}

func (c *composeStack) up() {
	c.t.Helper()
	if out, err := c.compose("up", "-d", "--wait"); err != nil {
		logs, _ := c.compose("logs", "--tail=50")
		c.t.Fatalf("docker compose up: %v\n%s\n%s", err, out, logs)
	}
}

// traceSpans returns the span count of each trace in an experiment, read with
// the MLflow client inside the server container.
func (c *composeStack) traceSpans(experiment string) []int {
	c.t.Helper()
	script := fmt.Sprintf(`
import json, mlflow
mlflow.set_tracking_uri("http://localhost:5000")
exp = mlflow.get_experiment_by_name(%q)
traces = mlflow.search_traces(experiment_ids=[exp.experiment_id], return_type="list", max_results=500) if exp else []
print(json.dumps([len(t.data.spans) for t in traces]))
`, experiment)
	out, err := c.compose("exec", "-T", "mlflow", "python", "-c", script)
	if err != nil {
		c.t.Fatalf("reading traces: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var spans []int
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &spans); err != nil {
		c.t.Fatalf("reading traces: %v\n%s", err, out)
	}
	return spans
}

func (c *composeStack) down() {
	if c.t.Failed() {
		logs, _ := c.compose("logs", "--tail=50", "mlflow")
		c.t.Logf("mlflow logs:\n%s", logs)
	}
	if os.Getenv("KINDLING_IT_KEEP") != "" {
		c.t.Logf("leaving compose project %s running in %s", c.project, c.dir)
		return
	}
	if out, err := c.compose("down", "-v"); err != nil {
		c.t.Logf("docker compose down: %v\n%s", err, out)
	}
}

func setEnv(t *testing.T, path, key, value string) {
	t.Helper()
	if err := spec.SetDotenv(path, key, value); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func randomSuffix() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
