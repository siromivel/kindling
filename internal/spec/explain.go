package spec

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Explain writes the effective middleware chain for every endpoint, or only
// the named one: which stages run in what order, the policy each pii stage
// applies, and whether caching is on. It's what the edge will do, sorted so
// the output can be diffed.
func (s *Spec) Explain(w io.Writer, endpoint string) error {
	m := s.Middleware
	if m == nil {
		return errors.New("kindling.yaml has no middleware section")
	}
	if endpoint != "" {
		if _, ok := s.Endpoints[endpoint]; !ok {
			return fmt.Errorf("unknown endpoint %q", endpoint)
		}
		s.explainChain(w, "endpoint "+endpoint, endpoint)
		return nil
	}
	fmt.Fprintln(w, "Requests pass through the stages top to bottom, responses bottom to top.")
	for _, name := range s.EndpointNames() {
		fmt.Fprintln(w)
		s.explainChain(w, "endpoint "+name, name)
	}
	fmt.Fprintln(w)
	s.explainChain(w, "any other model (not an endpoint in kindling.yaml)", "")
	return nil
}

func (s *Spec) explainChain(w io.Writer, title, endpoint string) {
	fmt.Fprintln(w, title)
	for i, e := range s.Middleware.Chain(endpoint) {
		st := e.Stage
		head := fmt.Sprintf("  %d. %-6s %-12s", i+1, st.Type, st.OnError)
		if !e.Enabled {
			fmt.Fprintf(w, "%s off (middleware.overrides.%s.%s)\n", head, endpoint, st.Type)
			continue
		}
		switch st.Type {
		case StagePII:
			fmt.Fprintf(w, "%s policy %s\n", head, e.Policy)
			pol := s.Middleware.PIIPolicies[e.Policy]
			fmt.Fprintf(w, "       roles: %s\n", strings.Join(pol.Roles, ", "))
			width := 0
			for typ := range pol.Entities {
				width = max(width, len(typ))
			}
			for _, typ := range slices.Sorted(maps.Keys(pol.Entities)) {
				ent := pol.Entities[typ]
				fmt.Fprintf(w, "       %-*s  %-7s  score >= %s\n", width, typ, ent.Operator, strconv.FormatFloat(ent.Threshold(), 'f', -1, 64))
			}
		case StageCache:
			details := []string{"exact", "ttl " + formatDuration(st.TTL), fmt.Sprintf("max_entries %d", st.MaxEntries)}
			if st.CacheSalt != "" {
				details = append(details, fmt.Sprintf("cache_salt %q", st.CacheSalt))
			}
			if st.AllowUnredactedKeys {
				details = append(details, "unredacted keys allowed")
			}
			route := "none"
			if fp := s.RouteFingerprint(endpoint); fp != "" {
				route = fp[:12]
			}
			details = append(details, "route "+route)
			fmt.Fprintf(w, "%s on: %s\n", head, strings.Join(details, ", "))
		default:
			fmt.Fprintln(w, head)
		}
	}
}

// formatDuration prints 1h rather than time.Duration's 1h0m0s.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}
