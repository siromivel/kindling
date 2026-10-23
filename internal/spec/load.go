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
func Load(path string, env Env) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, env)
}

// Parse decodes kindling.yaml content and resolves ${VAR} references.
func Parse(data []byte, env Env) (*Spec, error) {
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
	for _, name := range s.ConnectionNames() {
		c := s.Connections[name]
		expand("connections."+name+".api_key", &c.APIKey)
		expand("connections."+name+".base_url", &c.BaseURL)
		s.Connections[name] = c
	}
	for _, name := range s.ModelNames() {
		m := s.Models[name]
		expand("models."+name+".model", &m.Model)
		s.Models[name] = m
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
