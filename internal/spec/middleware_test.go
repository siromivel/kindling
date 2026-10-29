package spec

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// middlewareSpec is the example from docs/specs/middleware-v0-spec.md, with a
// third endpoint that uses the default policy.
const middlewareSpec = `
router: {type: mlflow}
connections:
  anthropic: {type: anthropic, api_key: "${ANTHROPIC_API_KEY}"}
  vllm: {type: openai-compatible, base_url: "http://host.docker.internal:8000/v1"}
models:
  claude: {connection: anthropic, model: claude-sonnet-5-5}
  qwen: {connection: vllm, model: Qwen/Qwen3.8-27B}
endpoints:
  chat:
    route:
      - {model: claude, weight: 80}
      - {model: qwen, weight: 20}
  local:
    route: [qwen]
  assist:
    route: [qwen]
    fallback: [claude]
middleware:
  listen: 127.0.0.1:${KINDLING_PROXY_PORT:-5060}
  stages:
    - type: pii
      on_error: fail_closed
      policy: standard
    - type: cache
      on_error: fail_open
      mode: exact
      ttl: 1h
      max_entries: 10000
  pii_policies:
    standard:
      roles: [user, system]
      entities:
        US_SSN:        { operator: replace, score_threshold: 0.3 }
        CREDIT_CARD:   { operator: replace, score_threshold: 0.3 }
        EMAIL_ADDRESS: { operator: replace }
        PHONE_NUMBER:  { operator: mask }
        PERSON:        { operator: keep }
    strict:
      roles: [user, system, assistant]
      entities:
        US_SSN:        { operator: replace, score_threshold: 0.3 }
        CREDIT_CARD:   { operator: replace, score_threshold: 0.3 }
        EMAIL_ADDRESS: { operator: replace }
        PHONE_NUMBER:  { operator: replace }
        PERSON:        { operator: replace, score_threshold: 0.5 }
        LOCATION:      { operator: replace }
  overrides:
    chat:
      pii:   { policy: strict }
      cache: { enabled: false }
    local:
      pii:   { enabled: false }
`

func middlewareFixture(t *testing.T, replacements ...string) *Spec {
	t.Helper()
	return parse(t, strings.NewReplacer(replacements...).Replace(middlewareSpec), map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test"})
}

func problemStrings(s *Spec) []string {
	var out []string
	for _, p := range s.Validate() {
		out = append(out, p.String())
	}
	return out
}

func TestMiddlewareParse(t *testing.T) {
	s := middlewareFixture(t)
	m := s.Middleware
	if m.Listen != "127.0.0.1:5060" {
		t.Errorf("listen = %q, want default port expanded", m.Listen)
	}
	if got := m.Stages[1].TTL.String(); got != "1h0m0s" {
		t.Errorf("ttl = %s", got)
	}
	if got := m.PIIPolicies["standard"].Entities["PHONE_NUMBER"].Threshold(); got != DefaultScoreThreshold {
		t.Errorf("default threshold = %v", got)
	}
	// The only finding is the notice about local.
	want := []string{"notice: middleware.overrides: pii is disabled for local; their requests reach the router unredacted"}
	if got := problemStrings(s); !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
}

func TestParseForEdgeSkipsCredentials(t *testing.T) {
	s, err := ParseForEdge([]byte(middlewareSpec), Env{})
	if err != nil {
		t.Fatalf("ParseForEdge without ANTHROPIC_API_KEY: %v", err)
	}
	if got := s.Connections["anthropic"].APIKey; got != "${ANTHROPIC_API_KEY}" {
		t.Errorf("api_key = %q, want it left unexpanded", got)
	}
	if ps := s.ValidateMiddleware(); HasErrors(ps) {
		t.Errorf("ValidateMiddleware: %v", ps)
	}
}

func TestValidateMiddleware(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  string
		new  string
		want string
	}{
		{"on_error required", "      on_error: fail_closed\n", "", "middleware.stages[0].on_error: required"},
		{"on_error value", "on_error: fail_closed", "on_error: closed", `must be fail_closed or fail_open, got "closed"`},
		{"unknown stage", "type: cache", "type: guard", `unknown stage type "guard"`},
		{"duplicate stage", "type: cache", "type: pii\n      policy: strict", "a pii stage is already configured"},
		{"setting for another type", "policy: standard", "policy: standard\n      ttl: 1h", "middleware.stages[0].ttl: not a pii stage setting"},
		{"typo in stage key", "max_entries: 10000", "max_entires: 10000", "middleware.stages[1].max_entires: not a cache stage setting"},
		{"cache needs ttl", "      ttl: 1h\n", "", "middleware.stages[1].ttl: required"},
		{"cache mode", "mode: exact", "mode: semantic", `must be exact, got "semantic"`},
		{"pii needs policy", "      policy: standard\n", "", "middleware.stages[0].policy: required"},
		{"undefined policy", "policy: standard\n", "policy: lenient\n", `middleware.stages[0].policy: unknown policy "lenient"`},
		{"unknown entity", "LOCATION:", "LOCATON:", `middleware.pii_policies.strict.entities.LOCATON: unknown entity type "LOCATON"`},
		{"unknown operator", "PERSON:        { operator: keep }", "PERSON:        { operator: hash }", `unknown operator "hash"`},
		{"operator required", "EMAIL_ADDRESS: { operator: replace }\n        PHONE_NUMBER:  { operator: mask }", "EMAIL_ADDRESS: {}\n        PHONE_NUMBER:  { operator: mask }", "standard.entities.EMAIL_ADDRESS.operator: required"},
		{"threshold range", "score_threshold: 0.5", "score_threshold: 5", "score_threshold: must be between 0 and 1"},
		{"mask on a name", "PERSON:        { operator: keep }", "PERSON:        { operator: mask }", "notice: middleware.pii_policies.standard.entities.PERSON.operator: mask keeps the last 4 characters, which gives away most of a PERSON value"},
		{"tool role", "roles: [user, system]", "roles: [user, tool]", "tool messages aren't scanned yet"},
		{"unused policy", "pii:   { policy: strict }", "pii:   { enabled: true }", "warning: middleware.pii_policies.strict: not used"},
		{"override unknown endpoint", "    local:\n      pii:", "    lcoal:\n      pii:", `middleware.overrides.lcoal: unknown endpoint "lcoal"`},
		{"override unknown stage", "cache: { enabled: false }", "guard: { enabled: false }", `middleware.overrides.chat.guard: unknown stage type "guard"`},
		{"override unknown policy", "{ policy: strict }", "{ policy: strcit }", `middleware.overrides.chat.pii.policy: unknown policy "strcit"`},
		{"override extra field", "cache: { enabled: false }", "cache: { enabled: false, ttl: 5m }", "middleware.overrides.chat.cache.ttl: overrides can only set enabled: false"},
		{"override policy on cache", "cache: { enabled: false }", "cache: { policy: strict }", "middleware.overrides.chat.cache.policy: overrides can only set enabled: false"},
		{"override can't reorder", "cache: { enabled: false }", "cache: { order: 0 }", "middleware.overrides.chat.cache.order: overrides can only set"},
		{"override policy while disabled", "pii:   { enabled: false }", "pii:   { enabled: false, policy: strict }", "can't select a policy for a stage that's disabled"},
		{"empty override", "cache: { enabled: false }", "cache: {}", "middleware.overrides.chat.cache: empty override"},
		{"override for missing stage", "    - type: cache\n      on_error: fail_open\n      mode: exact\n      ttl: 1h\n      max_entries: 10000\n", "", "middleware.overrides.chat.cache: no cache stage is configured"},
		{"public listen", "listen: 127.0.0.1:", "listen: 0.0.0.0:", "warning: middleware.listen: the edge has no auth"},
		{"bad upstream", "  stages:", "  upstream: mlflow:5000\n  stages:", "middleware.upstream: must be an http(s) URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(middlewareSpec, tc.old) {
				t.Fatalf("fixture doesn't contain %q", tc.old)
			}
			got := problemStrings(middlewareFixture(t, tc.old, tc.new))
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("problems %q don't mention %q", got, tc.want)
			}
		})
	}
}

func TestValidateStageOrder(t *testing.T) {
	cacheFirst := `
router: {type: mlflow}
middleware:
  stages:
    - {type: cache, on_error: fail_open, ttl: 1h, max_entries: 10, ALLOW}
    - {type: pii, on_error: fail_closed, policy: p}
  pii_policies:
    p: {roles: [user], entities: {EMAIL_ADDRESS: {operator: replace}}}
`
	for _, tc := range []struct {
		allow string
		want  bool // valid
	}{
		{"allow_unredacted_keys: false", false},
		{"allow_unredacted_keys: true", true},
	} {
		s := parse(t, strings.Replace(cacheFirst, "ALLOW", tc.allow, 1), nil)
		ps := s.Validate()
		if HasErrors(ps) == tc.want {
			t.Errorf("cache before pii with %s: problems %v", tc.allow, ps)
		}
		if !tc.want && !strings.Contains(ps[0].String(), "error: middleware.stages[0]: cache runs before pii") {
			t.Errorf("problem = %v", ps[0])
		}
	}
}

func TestChainOverrides(t *testing.T) {
	m := middlewareFixture(t).Middleware
	type step struct {
		typ     string
		enabled bool
		policy  string
	}
	summarize := func(endpoint string) []step {
		var out []step
		for _, e := range m.Chain(endpoint) {
			out = append(out, step{e.Stage.Type, e.Enabled, e.Policy})
		}
		return out
	}
	for endpoint, want := range map[string][]step{
		"chat":         {{"pii", true, "strict"}, {"cache", false, ""}},
		"local":        {{"pii", false, "standard"}, {"cache", true, ""}},
		"assist":       {{"pii", true, "standard"}, {"cache", true, ""}},
		"hand-made-ep": {{"pii", true, "standard"}, {"cache", true, ""}},
	} {
		if got := summarize(endpoint); !slices.Equal(got, want) {
			t.Errorf("Chain(%s) = %v, want %v", endpoint, got, want)
		}
	}

	// No merging: chat gets strict exactly, not strict layered on standard.
	strict := m.PIIPolicies["strict"]
	if _, ok := strict.Entities["LOCATION"]; !ok || strict.Entities["PHONE_NUMBER"].Operator != OperatorReplace {
		t.Errorf("strict policy = %+v", strict)
	}
}

func TestRouteFingerprint(t *testing.T) {
	base := middlewareFixture(t)
	fp := func(s *Spec, endpoint string) string { return s.RouteFingerprint(endpoint) }
	chat := fp(base, "chat")
	if chat == "" || chat == fp(base, "local") {
		t.Fatalf("chat fingerprint %q should be set and differ from local's", chat)
	}
	if fp(base, "not-an-endpoint") != "" {
		t.Error("non-endpoints should have no fingerprint")
	}

	for _, tc := range []struct {
		name    string
		old     string
		new     string
		changes bool
	}{
		{"weights", "weight: 80}\n      - {model: qwen, weight: 20}", "weight: 30}\n      - {model: qwen, weight: 70}", false},
		{"route order", "- {model: claude, weight: 80}\n      - {model: qwen, weight: 20}", "- {model: qwen, weight: 20}\n      - {model: claude, weight: 80}", false},
		{"credentials", "api_key: \"${ANTHROPIC_API_KEY}\"", "api_key: sk-ant-other", false},
		{"base url", "host.docker.internal:8000", "host.docker.internal:9000", false},
		{"model id", "model: Qwen/Qwen3.8-27B", "model: Qwen/Qwen3-32B", true},
		{"connection type", "vllm: {type: openai-compatible, base_url: \"http://host.docker.internal:8000/v1\"}", "vllm: {type: ollama}", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := middlewareFixture(t, tc.old, tc.new)
			if changed := fp(s, "chat") != chat; changed != tc.changes {
				t.Errorf("fingerprint changed = %t, want %t", changed, tc.changes)
			}
		})
	}

	// Fallback order is excluded too.
	s := middlewareFixture(t)
	e := s.Endpoints["assist"]
	before := fp(s, "assist")
	e.Route, e.Fallback = []Target{{Model: "claude"}}, []string{"qwen"}
	s.Endpoints["assist"] = e
	if fp(s, "assist") != before {
		t.Error("swapping route and fallback changed the fingerprint")
	}
}

func TestExplainGolden(t *testing.T) {
	s := middlewareFixture(t, "max_entries: 10000", "max_entries: 10000\n      cache_salt: \"2026-10\"")
	var buf bytes.Buffer
	if err := s.Explain(&buf, ""); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		var again bytes.Buffer
		s.Explain(&again, "")
		if again.String() != buf.String() {
			t.Fatal("explain output differs between runs")
		}
	}
	golden := filepath.Join("testdata", "explain.golden")
	if *update {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if buf.String() != string(want) {
		t.Errorf("explain output:\n%s\nwant (go test ./internal/spec -run Golden -update to accept):\n%s", buf.String(), want)
	}

	var one bytes.Buffer
	if err := s.Explain(&one, "chat"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(one.String(), "endpoint chat\n") || strings.Contains(one.String(), "endpoint local") {
		t.Errorf("explain chat:\n%s", one.String())
	}
	if err := s.Explain(&one, "nope"); err == nil {
		t.Error("explain of unknown endpoint should fail")
	}
}
