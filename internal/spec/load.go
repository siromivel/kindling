package spec

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Load reads a kindling.yaml file, resolving ${VAR} references against env.
func Load(path string, env Env) (*Spec, error) { return load(path, env, true) }

// Parse decodes kindling.yaml content and resolves ${VAR} references.
func Parse(data []byte, env Env) (*Spec, error) { return decode(data, env, true) }

// LoadForEdge is Load for `kindling serve`.
func LoadForEdge(path string, env Env) (*Spec, error) { return load(path, env, false) }

// ParseForEdge is Parse for the edge, which uses only each connection's type.
// Connections' api_key and base_url are left unexpanded, so the edge never
// holds credentials and their variables needn't be set where it runs.
func ParseForEdge(data []byte, env Env) (*Spec, error) { return decode(data, env, false) }

func load(path string, env Env, connections bool) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decode(data, env, connections)
}

func decode(data []byte, env Env, connections bool) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parsing kindling.yaml: %w", err)
	}

	var missing []string
	expand := func(field string, v *string) {
		out, m := interpolate(*v, env)
		*v = out
		for _, name := range m {
			missing = append(missing, fmt.Sprintf("%s (used by %s)", name, field))
		}
	}

	expand("router.url", &s.Router.URL)
	if connections {
		for _, name := range s.ConnectionNames() {
			c := s.Connections[name]
			expand("connections."+name+".api_key", &c.APIKey)
			expand("connections."+name+".base_url", &c.BaseURL)
			s.Connections[name] = c
		}
	}
	for _, name := range s.ModelNames() {
		m := s.Models[name]
		expand("models."+name+".model", &m.Model)
		s.Models[name] = m
	}
	if m := s.Middleware; m != nil {
		expand("middleware.listen", &m.Listen)
		expand("middleware.upstream", &m.Upstream)
		for i := range m.Stages {
			expand(fmt.Sprintf("middleware.stages[%d].cache_salt", i), &m.Stages[i].CacheSalt)
		}
		m.Upstream = strings.TrimRight(m.Upstream, "/")
	}
	if len(missing) > 0 {
		return nil, errors.New("unset environment variables:\n  " + strings.Join(missing, "\n  "))
	}

	if s.Router.URL == "" {
		s.Router.URL = DefaultRouterURL
	}
	s.Router.URL = strings.TrimRight(s.Router.URL, "/")
	return &s, nil
}
