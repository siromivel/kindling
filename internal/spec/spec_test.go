package spec

import (
	"slices"
	"strings"
	"testing"
)

func parse(t *testing.T, yaml string, vars map[string]string) *Spec {
	t.Helper()
	s, err := Parse([]byte(yaml), Env(vars))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const valid = `
router: {type: mlflow}
connections:
  openai: {type: openai, api_key: "${OPENAI_API_KEY}"}
  local: {type: openai-compatible, base_url: "http://host.docker.internal:${VLLM_PORT:-8000}/v1"}
models:
  gpt: {connection: openai, model: gpt-5}
  qwen: {connection: local, model: Qwen/Qwen3-32B}
endpoints:
  chat:
    route:
      - {model: gpt, weight: 70}
      - {model: qwen, weight: 30}
  local:
    route: [qwen]
    fallback: [gpt]
    usage_tracking: false
`

func TestParse(t *testing.T) {
	s := parse(t, valid, map[string]string{"OPENAI_API_KEY": "sk-test"})

	if got := s.Connections["openai"].APIKey; got != "sk-test" {
		t.Errorf("api_key = %q, want value from env", got)
	}
	if got := s.Connections["local"].BaseURL; got != "http://host.docker.internal:8000/v1" {
		t.Errorf("base_url = %q, want default port", got)
	}
	if s.Router.URL != DefaultRouterURL {
		t.Errorf("router.url = %q, want default", s.Router.URL)
	}
	if got := s.Endpoints["local"].Route; len(got) != 1 || got[0].Model != "qwen" {
		t.Errorf("shorthand route = %+v", got)
	}
	if s.Endpoints["local"].TrackUsage() || !s.Endpoints["chat"].TrackUsage() {
		t.Error("usage_tracking should default to true and honor false")
	}
	if ps := s.Validate(); len(ps) > 0 {
		t.Errorf("unexpected problems: %v", ps)
	}
}

func TestParseMissingVariable(t *testing.T) {
	_, err := Parse([]byte(valid), Env{})
	if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY (used by connections.openai.api_key)") {
		t.Fatalf("err = %v, want missing OPENAI_API_KEY", err)
	}
}

func TestParseUnknownField(t *testing.T) {
	_, err := Parse([]byte("router: {type: mlflow}\nconections: {}\n"), Env{})
	if err == nil || !strings.Contains(err.Error(), "conections") {
		t.Fatalf("err = %v, want unknown field error", err)
	}
}

func TestInterpolate(t *testing.T) {
	env := Env{"A": "1", "EMPTY": ""}
	for in, want := range map[string]string{
		"${A}":          "1",
		"x${A}y${A}":    "x1y1",
		"${EMPTY:-def}": "def",
		"${UNSET:-}":    "",
		"$$A and $HOME": "$A and $HOME",
		"${UNCLOSED":    "${UNCLOSED",
		"no references": "no references",
	} {
		got, missing := interpolate(in, env)
		if got != want || len(missing) > 0 {
			t.Errorf("interpolate(%q) = %q, %v; want %q", in, got, missing, want)
		}
	}
	if _, missing := interpolate("${EMPTY}${UNSET}", env); !slices.Equal(missing, []string{"EMPTY", "UNSET"}) {
		t.Errorf("missing = %v", missing)
	}
}

func TestWeights(t *testing.T) {
	for _, tc := range []struct {
		route []Target
		want  []int
	}{
		{[]Target{{Model: "a"}}, []int{100}},
		{[]Target{{Model: "a", Weight: 40}}, []int{100}},
		{[]Target{{Model: "a"}, {Model: "b"}, {Model: "c"}}, []int{34, 33, 33}},
		{[]Target{{Model: "a", Weight: 80}, {Model: "b", Weight: 20}}, []int{80, 20}},
	} {
		if got := (Endpoint{Route: tc.route}).Weights(); !slices.Equal(got, tc.want) {
			t.Errorf("Weights(%v) = %v, want %v", tc.route, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	base := func() *Spec { return parse(t, valid, map[string]string{"OPENAI_API_KEY": "sk-test"}) }
	for _, tc := range []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"unknown type", func(s *Spec) { s.Connections["openai"] = Connection{Type: "opneai"} }, `unknown type "opneai"`},
		{"missing key", func(s *Spec) { s.Connections["openai"] = Connection{Type: "openai"} }, "connections.openai.api_key: required"},
		{"compatible needs url", func(s *Spec) { s.Connections["local"] = Connection{Type: OpenAICompatible} }, "connections.local.base_url: required"},
		{"bad router url", func(s *Spec) { s.Router.URL = "localhost:5050" }, "router.url: must be an http(s) URL"},
		{"bad url", func(s *Spec) { s.Connections["local"] = Connection{Type: OpenAICompatible, BaseURL: "host:8000"} }, "must be an http(s) URL"},
		{"base_url forbidden", func(s *Spec) {
			s.Connections["openai"] = Connection{Type: "gemini", APIKey: "k", BaseURL: "https://x.dev"}
		}, "not supported"},
		{"localhost", func(s *Spec) {
			s.Connections["local"] = Connection{Type: OpenAICompatible, BaseURL: "http://localhost:8000/v1"}
		}, "warning: connections.local.base_url: localhost"},
		{"bad name", func(s *Spec) { s.Endpoints["my chat"] = s.Endpoints["chat"] }, `endpoints.my chat: name must match`},
		{"unknown connection", func(s *Spec) { s.Models["gpt"] = Model{Connection: "nope", Model: "x"} }, `unknown connection "nope"`},
		{"unknown model", func(s *Spec) { s.Endpoints["x"] = Endpoint{Route: []Target{{Model: "nope"}}} }, `unknown model "nope"`},
		{"empty route", func(s *Spec) { s.Endpoints["x"] = Endpoint{Fallback: []string{"gpt"}} }, "needs at least one model"},
		{"duplicate", func(s *Spec) { s.Endpoints["x"] = Endpoint{Route: []Target{{Model: "gpt"}}, Fallback: []string{"gpt"}} }, "more than once"},
		{"bad sum", func(s *Spec) {
			s.Endpoints["chat"] = Endpoint{Route: []Target{{Model: "gpt", Weight: 70}, {Model: "qwen", Weight: 20}}}
		}, "sum to 100 (got 90)"},
		{"partial weights", func(s *Spec) {
			s.Endpoints["chat"] = Endpoint{Route: []Target{{Model: "gpt", Weight: 100}, {Model: "qwen"}}}
		}, "weights must be set on every model"},
		{"unused model", func(s *Spec) { s.Models["spare"] = Model{Connection: "openai", Model: "gpt-5-mini"} }, "warning: models.spare: not used"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.mutate(s)
			var got []string
			for _, p := range s.Validate() {
				got = append(got, p.String())
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Errorf("problems %q don't mention %q", got, tc.want)
			}
		})
	}
}
