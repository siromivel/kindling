package spec

import (
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

type Severity string

const (
	Error   Severity = "error"
	Warning Severity = "warning"
)

// Problem is a validation finding, anchored to a path in kindling.yaml.
type Problem struct {
	Severity Severity
	Path     string
	Message  string
}

func (p Problem) String() string {
	return fmt.Sprintf("%s: %s: %s", p.Severity, p.Path, p.Message)
}

// HasErrors reports whether any problem is an error rather than a warning.
func HasErrors(problems []Problem) bool {
	return slices.ContainsFunc(problems, func(p Problem) bool { return p.Severity == Error })
}

// Names become URL path segments on most routers, so keep them boring.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Validate checks the spec for router-independent mistakes.
func (s *Spec) Validate() []Problem {
	// Errors and warnings are collected separately so errors come first,
	// each in spec order.
	var errs, warns []Problem
	add := func(sev Severity, path, format string, args ...any) {
		p := Problem{sev, path, fmt.Sprintf(format, args...)}
		if sev == Error {
			errs = append(errs, p)
		} else {
			warns = append(warns, p)
		}
	}
	checkName := func(path, name string) {
		if !namePattern.MatchString(name) {
			add(Error, path, "name must match %s", namePattern)
		}
	}
	// checkURL returns the parsed URL, or nil after reporting it as invalid.
	checkURL := func(path, raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			add(Error, path, "must be an http(s) URL, got %q", raw)
			return nil
		}
		return u
	}

	if s.Router.Type == "" {
		add(Error, "router.type", "required (e.g. mlflow)")
	}
	checkURL("router.url", s.Router.URL)

	for _, name := range s.ConnectionNames() {
		c := s.Connections[name]
		path := "connections." + name
		checkName(path, name)
		ct, ok := ConnectionTypes[c.Type]
		if !ok {
			add(Error, path+".type", "unknown type %q (known: %s)", c.Type, strings.Join(slices.Sorted(maps.Keys(ConnectionTypes)), ", "))
			continue
		}
		if ct.APIKeyRequired && c.APIKey == "" {
			add(Error, path+".api_key", "required for %s connections", c.Type)
		}
		switch {
		case ct.BaseURL == BaseURLRequired && c.BaseURL == "":
			add(Error, path+".base_url", "required for %s connections", c.Type)
		case ct.BaseURL == BaseURLForbidden && c.BaseURL != "":
			add(Error, path+".base_url", "not supported for %s connections", c.Type)
		}
		if c.BaseURL != "" {
			if u := checkURL(path+".base_url", c.BaseURL); u != nil {
				if host := u.Hostname(); host == "localhost" || net.ParseIP(host).IsLoopback() {
					add(Warning, path+".base_url", "%s is the router's own container when it runs in Docker; use host.docker.internal to reach the host", host)
				}
			}
		}
	}

	for _, name := range s.ModelNames() {
		m := s.Models[name]
		path := "models." + name
		checkName(path, name)
		if _, ok := s.Connections[m.Connection]; !ok {
			add(Error, path+".connection", "unknown connection %q", m.Connection)
		}
		if m.Model == "" {
			add(Error, path+".model", "required")
		}
	}

	used := map[string]bool{}
	for _, name := range s.EndpointNames() {
		e := s.Endpoints[name]
		path := "endpoints." + name
		checkName(path, name)
		if len(e.Route) == 0 {
			add(Error, path+".route", "needs at least one model")
		}

		seen := map[string]bool{}
		checkRef := func(p, model string) {
			if _, ok := s.Models[model]; !ok {
				add(Error, p, "unknown model %q", model)
			}
			if seen[model] {
				add(Error, p, "model %q appears more than once in this endpoint", model)
			}
			seen[model] = true
			used[model] = true
		}

		sum, weighted := 0, 0
		for i, t := range e.Route {
			p := fmt.Sprintf("%s.route[%d]", path, i)
			checkRef(p, t.Model)
			if t.Weight < 0 || t.Weight > 100 {
				add(Error, p+".weight", "must be between 1 and 100")
			}
			if t.Weight != 0 {
				weighted++
			}
			sum += t.Weight
		}
		if len(e.Route) > 1 && weighted > 0 && (weighted != len(e.Route) || sum != 100) {
			add(Error, path+".route", "weights must be set on every model and sum to 100 (got %d), or be omitted for an even split", sum)
		}
		for i, model := range e.Fallback {
			checkRef(fmt.Sprintf("%s.fallback[%d]", path, i), model)
		}
	}

	for _, name := range s.ModelNames() {
		if !used[name] {
			add(Warning, "models."+name, "not used by any endpoint")
		}
	}

	return append(errs, warns...)
}
