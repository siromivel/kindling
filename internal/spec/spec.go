// Package spec defines kindling.yaml: a router-agnostic description of the
// connections, models and endpoints a user wants their LLM router to serve.
package spec

import (
	"fmt"
	"maps"
	"slices"

	"go.yaml.in/yaml/v3"
)

// DefaultRouterURL is where kindling expects the router when kindling.yaml
// doesn't say otherwise; it matches the port bound by the bundled compose file.
const DefaultRouterURL = "http://localhost:5050"

type Spec struct {
	Router      Router                `yaml:"router"`
	Connections map[string]Connection `yaml:"connections"`
	Models      map[string]Model      `yaml:"models"`
	Endpoints   map[string]Endpoint   `yaml:"endpoints"`
	// Middleware is nil when kindling.yaml has no middleware section.
	Middleware *Middleware `yaml:"middleware"`
}

type Router struct {
	// Type selects the backend, e.g. "mlflow".
	Type string `yaml:"type"`
	// URL is where kindling reaches the router's management API.
	URL string `yaml:"url"`
}

// Connection is a set of credentials for one upstream API.
type Connection struct {
	// Type is a key of ConnectionTypes, e.g. "openai-compatible" or "anthropic".
	Type    string `yaml:"type"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
}

// EffectiveBaseURL returns the configured base URL, falling back to the
// connection type's default.
func (c Connection) EffectiveBaseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return ConnectionTypes[c.Type].DefaultBaseURL
}

// Model is a named model served through a connection.
type Model struct {
	Connection string `yaml:"connection"`
	// Model is the upstream model identifier, e.g. "gpt-5" or "qwen3:32b".
	Model string `yaml:"model"`
}

// Endpoint is what clients call. Requests are split across Route by weight;
// if the chosen model fails, Fallback models are tried in order.
type Endpoint struct {
	Route    []Target `yaml:"route"`
	Fallback []string `yaml:"fallback"`
	// UsageTracking records traces for every request. Defaults to true.
	UsageTracking *bool `yaml:"usage_tracking"`
}

// Target is one model in an endpoint's traffic split. In YAML it is either a
// bare model name or a {model, weight} mapping.
type Target struct {
	Model string `yaml:"model"`
	// Weight is a percentage of traffic; zero means "split evenly".
	Weight int `yaml:"weight"`
}

func (t *Target) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		t.Model = n.Value
		return nil
	}
	type plain Target
	return n.Decode((*plain)(t))
}

// TrackUsage reports whether usage tracking is enabled for the endpoint.
func (e Endpoint) TrackUsage() bool {
	return e.UsageTracking == nil || *e.UsageTracking
}

// Weights returns the traffic percentage for each route target, summing to 100.
// Unweighted routes are split evenly, with any remainder going to the first target.
func (e Endpoint) Weights() []int {
	weights := make([]int, len(e.Route))
	if len(e.Route) == 0 {
		return weights
	}
	explicit := false
	for i, t := range e.Route {
		weights[i] = t.Weight
		explicit = explicit || t.Weight != 0
	}
	if len(e.Route) == 1 {
		weights[0] = 100
	} else if !explicit {
		for i := range weights {
			weights[i] = 100 / len(weights)
		}
		weights[0] += 100 % len(weights)
	}
	return weights
}

// ConnectionNames returns connection names in a stable order.
func (s *Spec) ConnectionNames() []string { return slices.Sorted(maps.Keys(s.Connections)) }

// ModelNames returns model names in a stable order.
func (s *Spec) ModelNames() []string { return slices.Sorted(maps.Keys(s.Models)) }

// EndpointNames returns endpoint names in a stable order.
func (s *Spec) EndpointNames() []string { return slices.Sorted(maps.Keys(s.Endpoints)) }

func (t Target) String() string {
	if t.Weight == 0 {
		return t.Model
	}
	return fmt.Sprintf("%s (%d%%)", t.Model, t.Weight)
}
