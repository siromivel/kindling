package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultEdgeListen is where `kindling serve` listens when middleware.listen
// is unset: loopback only, like the gateway, because the edge has no auth.
const DefaultEdgeListen = "127.0.0.1:5060"

// Middleware configures the edge (`kindling serve`), a proxy in front of the
// router that passes every chat request through a chain of stages.
type Middleware struct {
	Listen string `yaml:"listen"`
	// Upstream is the OpenAI-compatible base URL the edge forwards to. It
	// defaults to the router's own chat API.
	Upstream string `yaml:"upstream"`
	// Stages run in this order on requests, and in reverse on responses.
	Stages      []Stage              `yaml:"stages"`
	PIIPolicies map[string]PIIPolicy `yaml:"pii_policies"`
	// Overrides narrow the stages for one endpoint, keyed by endpoint name and
	// then by stage type.
	Overrides map[string]map[string]Override `yaml:"overrides"`
}

// EffectiveListen returns the configured listen address or the default.
func (m *Middleware) EffectiveListen() string {
	if m.Listen != "" {
		return m.Listen
	}
	return DefaultEdgeListen
}

// Stage types.
const (
	StagePII   = "pii"
	StageCache = "cache"
)

// Values for a stage's on_error.
const (
	FailOpen   = "fail_open"
	FailClosed = "fail_closed"
)

// stageKeys lists the settings each stage type accepts besides type and on_error.
var stageKeys = map[string][]string{
	StagePII:   {"policy"},
	StageCache: {"mode", "ttl", "max_entries", "cache_salt", "allow_unredacted_keys"},
}

// Stage is one entry of middleware.stages. Settings for every stage type share
// this struct; validation rejects the ones that don't apply to Type.
type Stage struct {
	Type string `yaml:"type"`
	// OnError is FailOpen or FailClosed. It's required: what happens to a
	// request when a stage breaks must be a decision, not a default.
	OnError string `yaml:"on_error"`

	// Policy is the pii_policies entry every endpoint uses unless overridden.
	Policy string `yaml:"policy"`

	Mode       string        `yaml:"mode"`
	TTL        time.Duration `yaml:"ttl"`
	MaxEntries int           `yaml:"max_entries"`
	// CacheSalt is mixed into every key; changing it empties the cache.
	CacheSalt string `yaml:"cache_salt"`
	// AllowUnredactedKeys permits a cache stage before the pii stage.
	AllowUnredactedKeys bool `yaml:"allow_unredacted_keys"`

	keys []string // keys set in kindling.yaml, for per-type checks
}

func (st *Stage) UnmarshalYAML(n *yaml.Node) error {
	type plain Stage
	if err := n.Decode((*plain)(st)); err != nil {
		return err
	}
	st.keys = mappingKeys(n)
	return nil
}

// PIIPolicy is a complete, named set of PII rules. Policies never merge: an
// endpoint gets exactly the policy it selects.
type PIIPolicy struct {
	// Roles are the message roles whose content is scanned.
	Roles []string `yaml:"roles"`
	// Entities maps each Presidio entity type to detect to its rule. Entity
	// types that aren't listed aren't detected.
	Entities map[string]PIIEntity `yaml:"entities"`
}

// PII operators.
const (
	OperatorReplace = "replace"
	OperatorMask    = "mask"
	OperatorKeep    = "keep"
)

// DefaultScoreThreshold applies to entities without a score_threshold. It's
// fixed on purpose so a policy reads the same everywhere.
const DefaultScoreThreshold = 0.5

type PIIEntity struct {
	Operator string `yaml:"operator"`
	// ScoreThreshold is the minimum detection confidence, from 0 to 1. Lower
	// is more aggressive.
	ScoreThreshold *float64 `yaml:"score_threshold"`
}

// Threshold returns the entity's score threshold, or the default.
func (e PIIEntity) Threshold() float64 {
	if e.ScoreThreshold != nil {
		return *e.ScoreThreshold
	}
	return DefaultScoreThreshold
}

// PIIEntityTypes lists what Presidio's default English analyzer detects
// (GET /supportedentities?language=en on presidio-analyzer 2.2.362).
var PIIEntityTypes = []string{
	"CREDIT_CARD", "CRYPTO", "DATE_TIME", "EMAIL_ADDRESS", "IBAN_CODE", "IP_ADDRESS",
	"LOCATION", "MAC_ADDRESS", "MEDICAL_LICENSE", "NRP", "PERSON", "PHONE_NUMBER", "UK_NHS",
	"URL", "US_BANK_NUMBER", "US_DRIVER_LICENSE", "US_ITIN", "US_PASSPORT", "US_SSN",
}

// maskRevealing are entity types that mask suits badly: keeping the last 4
// characters gives away most of a name or address (John Smith becomes
// **** *mith). mask is meant for numbers like phones, cards and SSNs.
var maskRevealing = []string{"EMAIL_ADDRESS", "LOCATION", "NRP", "PERSON", "URL"}

// piiRoles are the message roles a policy can scan. Tool messages (and tool
// call arguments) are out of scope for now.
var piiRoles = []string{"system", "developer", "user", "assistant"}

// Override changes one stage for one endpoint. It can only turn the stage off
// or, for pii, pick a different policy.
type Override struct {
	Enabled *bool  `yaml:"enabled"`
	Policy  string `yaml:"policy"`

	keys []string
}

func (o *Override) UnmarshalYAML(n *yaml.Node) error {
	type plain Override
	if err := n.Decode((*plain)(o)); err != nil {
		return err
	}
	o.keys = mappingKeys(n)
	return nil
}

func mappingKeys(n *yaml.Node) []string {
	var keys []string
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			keys = append(keys, n.Content[i].Value)
		}
	}
	return keys
}

// HasStage reports whether a stage of the given type is configured.
func (m *Middleware) HasStage(typ string) bool {
	return m != nil && slices.ContainsFunc(m.Stages, func(st Stage) bool { return st.Type == typ })
}

// EffectiveStage is a configured stage as it applies to one endpoint.
type EffectiveStage struct {
	Stage Stage
	// Enabled is false when an override turns the stage off.
	Enabled bool
	// Policy is the pii policy in effect, after overrides.
	Policy string
}

// Chain returns the stages that apply to requests for a model, in order.
// Names that aren't endpoints (or have no overrides) get the global stages.
func (m *Middleware) Chain(model string) []EffectiveStage {
	var chain []EffectiveStage
	for _, st := range m.Stages {
		e := EffectiveStage{Stage: st, Enabled: true, Policy: st.Policy}
		if o, ok := m.Overrides[model][st.Type]; ok {
			if o.Enabled != nil {
				e.Enabled = *o.Enabled
			}
			if o.Policy != "" {
				e.Policy = o.Policy
			}
		}
		chain = append(chain, e)
	}
	return chain
}

// RouteFingerprint identifies which models can answer for an endpoint: the
// connection type and model id of each route and fallback model. Weights,
// fallback order and credentials are left out on purpose, because they don't
// change what an answer could be. It's "" for names that aren't endpoints.
func (s *Spec) RouteFingerprint(endpoint string) string {
	e, ok := s.Endpoints[endpoint]
	if !ok {
		return ""
	}
	var pairs [][2]string
	add := func(model string) {
		m := s.Models[model]
		pairs = append(pairs, [2]string{s.Connections[m.Connection].Type, m.Model})
	}
	for _, t := range e.Route {
		add(t.Model)
	}
	for _, m := range e.Fallback {
		add(m)
	}
	slices.SortFunc(pairs, func(a, b [2]string) int {
		return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1])
	})
	data, _ := json.Marshal(slices.Compact(pairs))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ValidateMiddleware checks only the middleware section, for the edge, which
// doesn't use the rest.
func (s *Spec) ValidateMiddleware() []Problem {
	var ps problems
	s.validateMiddleware(ps.add)
	return ps.list()
}

func (s *Spec) validateMiddleware(add addFunc) {
	m := s.Middleware
	if m == nil {
		return
	}

	if host, _, err := net.SplitHostPort(m.EffectiveListen()); err != nil {
		add(Error, "middleware.listen", "must be host:port, got %q", m.Listen)
	} else if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		add(Warning, "middleware.listen", "the edge has no auth; keep it on a loopback address unless something in front of it handles that")
	}
	if m.Upstream != "" {
		checkURL(add, "middleware.upstream", m.Upstream)
	}

	if len(m.Stages) == 0 {
		add(Error, "middleware.stages", "needs at least one stage (%s)", strings.Join(slices.Sorted(maps.Keys(stageKeys)), ", "))
	}
	index := map[string]int{}
	for i, st := range m.Stages {
		path := fmt.Sprintf("middleware.stages[%d]", i)
		allowed, ok := stageKeys[st.Type]
		if !ok {
			add(Error, path+".type", "unknown stage type %q (known: %s)", st.Type, strings.Join(slices.Sorted(maps.Keys(stageKeys)), ", "))
			continue
		}
		if _, dup := index[st.Type]; dup {
			add(Error, path, "a %s stage is already configured; each stage type runs at most once", st.Type)
			continue
		}
		index[st.Type] = i

		for _, k := range st.keys {
			if k != "type" && k != "on_error" && !slices.Contains(allowed, k) {
				add(Error, path+"."+k, "not a %s stage setting (allowed: type, on_error, %s)", st.Type, strings.Join(allowed, ", "))
			}
		}
		switch st.OnError {
		case FailOpen, FailClosed:
		case "":
			add(Error, path+".on_error", "required: %s or %s, for what happens to requests when this stage fails", FailClosed, FailOpen)
		default:
			add(Error, path+".on_error", "must be %s or %s, got %q", FailClosed, FailOpen, st.OnError)
		}

		switch st.Type {
		case StagePII:
			if st.Policy == "" {
				add(Error, path+".policy", "required: the pii_policies entry endpoints use unless overridden")
			} else if _, ok := m.PIIPolicies[st.Policy]; !ok {
				add(Error, path+".policy", "unknown policy %q", st.Policy)
			}
		case StageCache:
			if st.Mode != "" && st.Mode != "exact" {
				add(Error, path+".mode", "must be exact, got %q", st.Mode)
			}
			if st.TTL <= 0 {
				add(Error, path+".ttl", "required: how long answers stay cached, e.g. 1h")
			}
			if st.MaxEntries <= 0 {
				add(Error, path+".max_entries", "required: the most responses to keep, e.g. 10000")
			}
		}
	}

	if c, ok := index[StageCache]; ok {
		p, hasPII := index[StagePII]
		allow := m.Stages[c].AllowUnredactedKeys
		switch {
		case hasPII && c < p && !allow:
			add(Error, fmt.Sprintf("middleware.stages[%d]", c),
				"cache runs before pii, so cache keys and stored responses would include unredacted PII; put pii first, or set allow_unredacted_keys: true to accept that")
		case allow && (!hasPII || c > p):
			add(Warning, fmt.Sprintf("middleware.stages[%d].allow_unredacted_keys", c), "has no effect unless cache runs before pii")
		}
	}

	used := map[string]bool{}
	if p, ok := index[StagePII]; ok {
		used[m.Stages[p].Policy] = true
	}
	for _, name := range slices.Sorted(maps.Keys(m.PIIPolicies)) {
		pol := m.PIIPolicies[name]
		path := "middleware.pii_policies." + name
		if !namePattern.MatchString(name) {
			add(Error, path, "name must match %s", namePattern)
		}
		if len(pol.Roles) == 0 {
			add(Error, path+".roles", "needs at least one role (%s)", strings.Join(piiRoles, ", "))
		}
		for i, r := range pol.Roles {
			switch {
			case slices.Contains(piiRoles, r):
			case r == "tool":
				add(Error, fmt.Sprintf("%s.roles[%d]", path, i), "tool messages aren't scanned yet")
			default:
				add(Error, fmt.Sprintf("%s.roles[%d]", path, i), "unknown role %q (known: %s)", r, strings.Join(piiRoles, ", "))
			}
		}
		if len(pol.Entities) == 0 {
			add(Error, path+".entities", "needs at least one entity type")
		}
		for _, typ := range slices.Sorted(maps.Keys(pol.Entities)) {
			e := pol.Entities[typ]
			ep := path + ".entities." + typ
			if !slices.Contains(PIIEntityTypes, typ) {
				add(Error, ep, "unknown entity type %q (known: %s)", typ, strings.Join(PIIEntityTypes, ", "))
			}
			switch e.Operator {
			case OperatorReplace, OperatorMask, OperatorKeep:
			case "":
				add(Error, ep+".operator", "required: replace, mask or keep")
			default:
				add(Error, ep+".operator", "unknown operator %q (known: replace, mask, keep)", e.Operator)
			}
			if e.Operator == OperatorMask && slices.Contains(maskRevealing, typ) {
				add(Notice, ep+".operator", "mask keeps the last 4 characters, which gives away most of a %s value (John Smith becomes **** *mith); replace hides it", typ)
			}
			if t := e.Threshold(); t < 0 || t > 1 {
				add(Error, ep+".score_threshold", "must be between 0 and 1")
			}
		}
	}

	var piiOff []string
	for _, endpoint := range slices.Sorted(maps.Keys(m.Overrides)) {
		path := "middleware.overrides." + endpoint
		if _, ok := s.Endpoints[endpoint]; !ok {
			add(Error, path, "unknown endpoint %q", endpoint)
		}
		for _, typ := range slices.Sorted(maps.Keys(m.Overrides[endpoint])) {
			o := m.Overrides[endpoint][typ]
			op := path + "." + typ
			if _, ok := stageKeys[typ]; !ok {
				add(Error, op, "unknown stage type %q", typ)
				continue
			}
			if _, ok := index[typ]; !ok {
				add(Error, op, "no %s stage is configured", typ)
				continue
			}
			allowed := "enabled: false"
			if typ == StagePII {
				allowed += " or policy"
			}
			for _, k := range o.keys {
				if k != "enabled" && (k != "policy" || typ != StagePII) {
					add(Error, op+"."+k, "overrides can only set %s", allowed)
				}
			}
			if len(o.keys) == 0 {
				add(Error, op, "empty override; set %s", allowed)
			}
			disabled := o.Enabled != nil && !*o.Enabled
			if o.Policy != "" && typ == StagePII {
				used[o.Policy] = true
				if _, ok := m.PIIPolicies[o.Policy]; !ok {
					add(Error, op+".policy", "unknown policy %q", o.Policy)
				}
				if disabled {
					add(Error, op+".policy", "can't select a policy for a stage that's disabled")
				}
			}
			if typ == StagePII && disabled {
				piiOff = append(piiOff, endpoint)
			}
		}
	}

	for _, name := range slices.Sorted(maps.Keys(m.PIIPolicies)) {
		if !used[name] {
			add(Warning, "middleware.pii_policies."+name, "not used by the pii stage or any override")
		}
	}
	if len(piiOff) > 0 {
		add(Notice, "middleware.overrides", "pii is disabled for %s; their requests reach the router unredacted", strings.Join(piiOff, ", "))
	}
}
