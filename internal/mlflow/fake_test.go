package mlflow

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGateway is an in-memory stand-in for MLflow's gateway REST API that
// follows its update semantics (empty maps and unset fields mean "unchanged").
type fakeGateway struct {
	mu        sync.Mutex
	nextID    int
	secrets   map[string]*Secret
	values    map[string]map[string]string // secret ID → plaintext
	models    map[string]*ModelDefinition
	endpoints map[string]*Endpoint
	calls     []string
}

func newFakeGateway(t *testing.T) (*fakeGateway, *Client) {
	f := &fakeGateway{
		secrets:   map[string]*Secret{},
		values:    map[string]map[string]string{},
		models:    map[string]*ModelDefinition{},
		endpoints: map[string]*Endpoint{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewClient(srv.URL)
}

func (f *fakeGateway) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeGateway) mask(v map[string]string) map[string]string {
	out := map[string]string{}
	for k, s := range v {
		out[k] = maskValue(s)
	}
	return out
}

func (f *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	op := strings.TrimPrefix(r.URL.Path, "/api/3.0/mlflow/gateway/")
	if r.Method != http.MethodGet {
		f.calls = append(f.calls, op)
	}
	var req map[string]json.RawMessage
	if r.Body != nil && r.Method != http.MethodGet {
		json.NewDecoder(r.Body).Decode(&req)
	}
	str := func(k string) string {
		var s string
		json.Unmarshal(req[k], &s)
		return s
	}
	strMap := func(k string) map[string]string {
		m := map[string]string{}
		json.Unmarshal(req[k], &m)
		return m
	}
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		reply(map[string]string{"error_code": "INVALID_PARAMETER_VALUE", "message": msg})
	}

	switch op {
	case "secrets/list":
		var out []Secret
		for _, s := range f.secrets {
			out = append(out, *s)
		}
		reply(map[string]any{"secrets": out})
	case "secrets/create":
		for _, s := range f.secrets {
			if s.Name == str("secret_name") {
				fail(400, "secret exists")
				return
			}
		}
		s := &Secret{ID: f.id("secret"), Name: str("secret_name"), Provider: str("provider"),
			AuthConfig: strMap("auth_config"), CreatedBy: str("created_by")}
		f.values[s.ID] = strMap("secret_value")
		s.MaskedValues = f.mask(f.values[s.ID])
		f.secrets[s.ID] = s
		reply(map[string]any{"secret": s})
	case "secrets/update":
		s := f.secrets[str("secret_id")]
		if v := strMap("secret_value"); len(v) > 0 {
			f.values[s.ID] = v
			s.MaskedValues = f.mask(v)
		}
		if ac := strMap("auth_config"); len(ac) > 0 {
			s.AuthConfig = map[string]string{}
			for k, v := range ac {
				if v != "" {
					s.AuthConfig[k] = v
				}
			}
		}
		reply(map[string]any{"secret": s})
	case "secrets/delete":
		delete(f.secrets, str("secret_id"))
		reply(struct{}{})

	case "model-definitions/list":
		var out []ModelDefinition
		for _, m := range f.models {
			out = append(out, *m)
		}
		reply(map[string]any{"model_definitions": out})
	case "model-definitions/create":
		m := &ModelDefinition{ID: f.id("model"), Name: str("name"), SecretID: str("secret_id"),
			Provider: str("provider"), ModelName: str("model_name"), CreatedBy: str("created_by")}
		if f.secrets[m.SecretID] == nil {
			fail(404, "no such secret "+m.SecretID)
			return
		}
		f.models[m.ID] = m
		reply(map[string]any{"model_definition": m})
	case "model-definitions/update":
		m := f.models[str("model_definition_id")]
		if v := str("secret_id"); v != "" {
			m.SecretID = v
		}
		if v := str("provider"); v != "" {
			m.Provider = v
		}
		if v := str("model_name"); v != "" {
			m.ModelName = v
		}
		reply(map[string]any{"model_definition": m})
	case "model-definitions/delete":
		delete(f.models, str("model_definition_id"))
		reply(struct{}{})

	case "endpoints/list":
		var out []Endpoint
		for _, e := range f.endpoints {
			out = append(out, *e)
		}
		reply(map[string]any{"endpoints": out})
	case "endpoints/create", "endpoints/update":
		var body EndpointRequest
		raw, _ := json.Marshal(req)
		json.Unmarshal(raw, &body)
		e := &Endpoint{ID: f.id("endpoint"), Name: body.Name, CreatedBy: body.CreatedBy}
		if op == "endpoints/update" {
			e = f.endpoints[body.ID]
		}
		e.ModelMappings = nil
		for _, mc := range body.ModelConfigs {
			if f.models[mc.ModelDefinitionID] == nil {
				fail(404, "no such model "+mc.ModelDefinitionID)
				return
			}
			e.ModelMappings = append(e.ModelMappings, ModelMapping{
				ModelDefinitionID: mc.ModelDefinitionID, LinkageType: mc.LinkageType,
				Weight: mc.Weight, FallbackOrder: mc.FallbackOrder,
			})
		}
		if body.RoutingStrategy != "" {
			e.RoutingStrategy = body.RoutingStrategy
		}
		if body.FallbackConfig != nil {
			e.FallbackConfig = body.FallbackConfig
		}
		e.UsageTracking = body.UsageTracking
		f.endpoints[e.ID] = e
		reply(map[string]any{"endpoint": e})
	case "endpoints/delete":
		delete(f.endpoints, str("endpoint_id"))
		reply(struct{}{})
	default:
		fail(404, "unknown op "+op)
	}
}

func (f *fakeGateway) secretValue(name string) map[string]string {
	for _, s := range f.secrets {
		if s.Name == name {
			return maps.Clone(f.values[s.ID])
		}
	}
	return nil
}
