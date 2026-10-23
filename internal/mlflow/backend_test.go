package mlflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/siromivel/kindling/internal/backend"
	"github.com/siromivel/kindling/internal/spec"
)

const testSpec = `
router: {type: mlflow}
connections:
  openai: {type: openai, api_key: sk-proj-abcdefgh1234}
  vllm: {type: openai-compatible, base_url: "http://host.docker.internal:8000/v1"}
  ollama: {type: ollama}
models:
  gpt: {connection: openai, model: gpt-5}
  qwen: {connection: vllm, model: Qwen/Qwen3-32B}
  llama: {connection: ollama, model: "llama3.3:70b"}
endpoints:
  chat:
    route:
      - {model: gpt, weight: 80}
      - {model: qwen, weight: 20}
    fallback: [llama]
  local:
    route: [qwen]
`

func mustParse(t *testing.T, y string) *spec.Spec {
	t.Helper()
	s, err := spec.Parse([]byte(y), spec.Env{})
	if err != nil {
		t.Fatal(err)
	}
	if ps := s.Validate(); spec.HasErrors(ps) {
		t.Fatal(ps)
	}
	return s
}

func plan(t *testing.T, b *Backend, s *spec.Spec, opts backend.Options) *backend.Plan {
	t.Helper()
	p, err := b.Plan(context.Background(), s, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func apply(t *testing.T, b *Backend, s *spec.Spec, opts backend.Options) []string {
	t.Helper()
	p := plan(t, b, s, opts)
	if err := p.Apply(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range p.Changes {
		out = append(out, c.String())
	}
	return out
}

func TestApplyFromScratchThenNoop(t *testing.T) {
	fake, client := newFakeGateway(t)
	b := &Backend{Client: client}
	s := mustParse(t, testSpec)

	changes := apply(t, b, s, backend.Options{})
	want := []string{
		"+ connection ollama", "+ connection openai", "+ connection vllm",
		"+ model gpt (openai/gpt-5)", "+ model llama (ollama/llama3.3:70b)", "+ model qwen (vllm/Qwen/Qwen3-32B)",
		"+ endpoint chat ([gpt 80%, qwen 20%] fallback [llama])", "+ endpoint local ([qwen 100%])",
	}
	if !slices.Equal(changes, want) {
		t.Errorf("changes:\n  %s\nwant:\n  %s", strings.Join(changes, "\n  "), strings.Join(want, "\n  "))
	}

	// Connections map to MLflow providers, with placeholder keys for local servers.
	for _, s := range fake.secrets {
		switch s.Name {
		case "vllm":
			if s.Provider != "openai" || s.AuthConfig["api_base"] != "http://host.docker.internal:8000/v1" {
				t.Errorf("vllm secret = %+v", s)
			}
			if v := fake.secretValue("vllm")["api_key"]; v != noAPIKey {
				t.Errorf("vllm api_key = %q", v)
			}
		case "ollama":
			if s.Provider != "ollama" || s.AuthConfig["api_base"] != "http://host.docker.internal:11434/v1" {
				t.Errorf("ollama secret = %+v", s)
			}
		case "openai":
			if len(s.AuthConfig) != 0 {
				t.Errorf("openai auth_config = %v, want none", s.AuthConfig)
			}
		}
	}
	for _, e := range fake.endpoints {
		if e.Name == "chat" {
			if e.RoutingStrategy != trafficSplit || e.FallbackConfig == nil || e.FallbackConfig.MaxAttempts != 1 {
				t.Errorf("chat routing = %q %+v", e.RoutingStrategy, e.FallbackConfig)
			}
			if !e.UsageTracking {
				t.Error("usage tracking should default on")
			}
		}
	}

	if changes := apply(t, b, s, backend.Options{}); len(changes) != 0 {
		t.Errorf("second apply should be a no-op, got %v", changes)
	}
}

func TestApplyUpdates(t *testing.T) {
	fake, client := newFakeGateway(t)
	b := &Backend{Client: client}
	apply(t, b, mustParse(t, testSpec), backend.Options{})

	updated := strings.NewReplacer(
		"sk-proj-abcdefgh1234", "sk-proj-newkey995678",
		"8000/v1", "8001/v1",
		"model: gpt-5}", "model: gpt-5.1}",
		"{model: gpt, weight: 80}", "{model: gpt, weight: 60}",
		"{model: qwen, weight: 20}", "{model: qwen, weight: 40}",
	).Replace(testSpec) + "    usage_tracking: false\n"

	changes := apply(t, b, mustParse(t, updated), backend.Options{})
	want := []string{
		"~ connection openai (credentials changed)",
		"~ connection vllm (base_url: http://host.docker.internal:8000/v1 → http://host.docker.internal:8001/v1)",
		"~ model gpt (model: gpt-5 → gpt-5.1)",
		"~ endpoint chat ([gpt 80%, qwen 20%] fallback [llama] → [gpt 60%, qwen 40%] fallback [llama])",
		"~ endpoint local (usage_tracking: true → false)",
	}
	if !slices.Equal(changes, want) {
		t.Errorf("changes:\n  %s\nwant:\n  %s", strings.Join(changes, "\n  "), strings.Join(want, "\n  "))
	}
	if v := fake.secretValue("openai")["api_key"]; v != "sk-proj-newkey995678" {
		t.Errorf("openai key = %q", v)
	}
	if changes := apply(t, b, mustParse(t, updated), backend.Options{}); len(changes) != 0 {
		t.Errorf("re-apply should be a no-op, got %v", changes)
	}
}

func TestRotateSecrets(t *testing.T) {
	_, client := newFakeGateway(t)
	b := &Backend{Client: client}
	s := mustParse(t, testSpec)
	apply(t, b, s, backend.Options{})

	changes := apply(t, b, s, backend.Options{RotateSecrets: true})
	if len(changes) != 3 || !strings.Contains(changes[0], "credentials re-sent") {
		t.Errorf("changes = %v", changes)
	}
}

func TestClearBaseURL(t *testing.T) {
	fake, client := newFakeGateway(t)
	b := &Backend{Client: client}
	withBase := strings.Replace(testSpec, "{type: openai, api_key: sk-proj-abcdefgh1234}",
		"{type: openai, api_key: sk-proj-abcdefgh1234, base_url: \"https://proxy.example.com/v1\"}", 1)
	apply(t, b, mustParse(t, withBase), backend.Options{})

	changes := apply(t, b, mustParse(t, testSpec), backend.Options{})
	if !slices.Equal(changes, []string{"~ connection openai (base_url: https://proxy.example.com/v1 → (none))"}) {
		t.Errorf("changes = %v", changes)
	}
	for _, s := range fake.secrets {
		if s.Name == "openai" && len(s.AuthConfig) != 0 {
			t.Errorf("auth_config not cleared: %v", s.AuthConfig)
		}
	}
}

func TestPrune(t *testing.T) {
	fake, client := newFakeGateway(t)
	b := &Backend{Client: client}
	apply(t, b, mustParse(t, testSpec), backend.Options{})

	// Something set up by hand in the MLflow UI must never be pruned.
	if _, err := client.CreateSecret(context.Background(), SecretRequest{Name: "manual", Value: map[string]string{"api_key": "x"}, Provider: "openai"}); err != nil {
		t.Fatal(err)
	}

	smaller := `
router: {type: mlflow}
connections:
  openai: {type: openai, api_key: sk-proj-abcdefgh1234}
models:
  gpt: {connection: openai, model: gpt-5}
endpoints:
  chat: {route: [gpt]}
`
	s := mustParse(t, smaller)
	p := plan(t, b, s, backend.Options{})
	wantUnmanaged := []string{"endpoint local", "model llama", "model qwen", "connection ollama", "connection vllm"}
	if !slices.Equal(p.Unmanaged, wantUnmanaged) {
		t.Errorf("unmanaged = %v, want %v", p.Unmanaged, wantUnmanaged)
	}

	changes := apply(t, b, s, backend.Options{Prune: true})
	want := []string{
		"~ endpoint chat ([gpt 80%, qwen 20%] fallback [llama] → [gpt 100%])",
		"- endpoint local", "- model llama", "- model qwen", "- connection ollama", "- connection vllm",
	}
	if !slices.Equal(changes, want) {
		t.Errorf("changes:\n  %s\nwant:\n  %s", strings.Join(changes, "\n  "), strings.Join(want, "\n  "))
	}
	if len(fake.secrets) != 2 || fake.secretValue("manual") == nil {
		t.Errorf("secrets after prune: %v", fake.secrets)
	}
}

func TestRefusesObjectsNotCreatedByKindling(t *testing.T) {
	_, client := newFakeGateway(t)
	b := &Backend{Client: client}
	if _, err := client.CreateSecret(context.Background(), SecretRequest{Name: "openai", Value: map[string]string{"api_key": "x"}, Provider: "openai"}); err != nil {
		t.Fatal(err)
	}
	_, err := b.Plan(context.Background(), mustParse(t, testSpec), backend.Options{})
	if err == nil || !strings.Contains(err.Error(), `connection "openai" already exists in MLflow but wasn't created by kindling`) {
		t.Fatalf("err = %v", err)
	}
}

func TestRequiredEnv(t *testing.T) {
	b := &Backend{}
	got := b.RequiredEnv(mustParse(t, testSpec))
	if len(got) != 2 || got[0].Value != "true" || got[1].Value != "http,https" ||
		!strings.Contains(got[0].Reason, "ollama, vllm") {
		t.Errorf("RequiredEnv = %+v", got)
	}

	hosted := mustParse(t, `
router: {type: mlflow}
connections:
  openai: {type: openai, api_key: sk-123456789}
models:
  gpt: {connection: openai, model: gpt-5}
endpoints:
  chat: {route: [gpt]}
`)
	if got := b.RequiredEnv(hosted); len(got) != 0 {
		t.Errorf("hosted-only RequiredEnv = %+v, want none", got)
	}
}

func TestNeedsPrivateNetwork(t *testing.T) {
	for url, want := range map[string][2]bool{
		"https://api.openai.com/v1":           {false, false},
		"http://host.docker.internal:8000/v1": {true, true},
		"http://vllm:8000/v1":                 {true, true},
		"https://10.0.0.5/v1":                 {true, false},
		"https://gpu-box.local/v1":            {true, false},
		"http://203.0.113.10/v1":              {false, true},
	} {
		p, i := needsPrivateNetwork(url)
		if [2]bool{p, i} != want {
			t.Errorf("needsPrivateNetwork(%s) = %v %v, want %v", url, p, i, want)
		}
	}
}

func TestMaskValue(t *testing.T) {
	for in, want := range map[string]string{
		"short":                "***",
		"sk-proj-abcdefgh1234": "sk-...1234",
		noAPIKey:               "kin...-key",
	} {
		if got := maskValue(in); got != want {
			t.Errorf("maskValue(%q) = %q, want %q", in, got, want)
		}
	}
}
