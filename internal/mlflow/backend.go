package mlflow

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/siromivel/kindling/internal/backend"
	"github.com/siromivel/kindling/internal/spec"
)

// owner marks objects kindling created (via their created_by field), so it
// never touches or prunes anything set up by hand in the MLflow UI.
const owner = "kindling"

type Backend struct {
	Client *Client
}

func New(s *spec.Spec) *Backend {
	return &Backend{Client: NewClient(s.Router.URL)}
}

func (b *Backend) RequiredEnv(s *spec.Spec) []backend.EnvRequirement {
	var private, insecure []string
	for _, name := range s.ConnectionNames() {
		p, i := needsPrivateNetwork(s.Connections[name].EffectiveBaseURL())
		if p {
			private = append(private, name)
		}
		if i {
			insecure = append(insecure, name)
		}
	}
	var reqs []backend.EnvRequirement
	if len(private) > 0 {
		reqs = append(reqs, backend.EnvRequirement{
			Key:    "MLFLOW_GATEWAY_API_BASE_ALLOW_PRIVATE_IPS",
			Value:  "true",
			Reason: "connections on private addresses: " + strings.Join(private, ", "),
		})
	}
	if len(insecure) > 0 {
		reqs = append(reqs, backend.EnvRequirement{
			Key:    "MLFLOW_GATEWAY_API_BASE_ALLOWED_SCHEMES",
			Value:  "http,https",
			Reason: "connections using plain http: " + strings.Join(insecure, ", "),
		})
	}
	return reqs
}

func (b *Backend) Plan(ctx context.Context, s *spec.Spec, opts backend.Options) (*backend.Plan, error) {
	secrets, err := b.Client.ListSecrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing connections: %w", err)
	}
	models, err := b.Client.ListModelDefinitions(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing models: %w", err)
	}
	endpoints, err := b.Client.ListEndpoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing endpoints: %w", err)
	}

	p := &planner{
		c:          b.Client,
		s:          s,
		opts:       opts,
		secrets:    map[string]Secret{},
		models:     map[string]ModelDefinition{},
		endpoints:  map[string]Endpoint{},
		secretIDs:  map[string]string{},
		modelIDs:   map[string]string{},
		secretName: map[string]string{},
		modelName:  map[string]string{},
	}
	for _, x := range secrets {
		p.secrets[x.Name], p.secretIDs[x.Name], p.secretName[x.ID] = x, x.ID, x.Name
	}
	for _, x := range models {
		p.models[x.Name], p.modelIDs[x.Name], p.modelName[x.ID] = x, x.ID, x.Name
	}
	for _, x := range endpoints {
		p.endpoints[x.Name] = x
	}

	p.planSecrets()
	p.planModels()
	p.planEndpoints()
	p.planPrune()
	if err := errors.Join(p.errs...); err != nil {
		return nil, err
	}
	return &p.plan, nil
}

type planner struct {
	c    *Client
	s    *spec.Spec
	opts backend.Options

	secrets   map[string]Secret
	models    map[string]ModelDefinition
	endpoints map[string]Endpoint

	// Name → ID, updated as changes run so later changes can find objects
	// created by earlier ones.
	secretIDs, modelIDs map[string]string
	// ID → name, for describing existing state.
	secretName, modelName map[string]string

	plan backend.Plan
	errs []error
}

func (p *planner) add(c backend.Change) { p.plan.Changes = append(p.plan.Changes, c) }

func (p *planner) owned(kind, name, createdBy string) bool {
	if createdBy == owner {
		return true
	}
	p.errs = append(p.errs, fmt.Errorf(
		"%s %q already exists in MLflow but wasn't created by kindling; rename it in kindling.yaml or delete it in the MLflow UI",
		kind, name))
	return false
}

func (p *planner) planSecrets() {
	for _, name := range p.s.ConnectionNames() {
		want := secretFor(p.s.Connections[name])
		have, exists := p.secrets[name]
		if !exists {
			p.add(backend.Change{Action: backend.Create, Kind: "connection", Name: name, Run: func(ctx context.Context) error {
				created, err := p.c.CreateSecret(ctx, SecretRequest{
					Name: name, Value: want.Value, Provider: want.Provider, AuthConfig: want.AuthConfig, CreatedBy: owner,
				})
				p.secretIDs[name] = created.ID
				return err
			}})
			continue
		}
		if !p.owned("connection", name, have.CreatedBy) {
			continue
		}
		if have.Provider != want.Provider {
			p.errs = append(p.errs, fmt.Errorf(
				"connection %q changed provider (%s → %s), which MLflow can't update in place; give the connection a new name",
				name, have.Provider, want.Provider))
			continue
		}

		req := SecretRequest{ID: have.ID, UpdatedBy: owner}
		var details []string
		if p.opts.RotateSecrets {
			req.Value = want.Value
			details = append(details, "credentials re-sent")
		} else if !masksMatch(have.MaskedValues, want.Value) {
			req.Value = want.Value
			details = append(details, "credentials changed")
		}
		if haveBase, wantBase := trimSlash(have.AuthConfig["api_base"]), trimSlash(want.AuthConfig["api_base"]); haveBase != wantBase {
			// An empty map means "unchanged" to MLflow; a blank api_base clears it.
			req.AuthConfig = map[string]string{"api_base": want.AuthConfig["api_base"]}
			details = append(details, fmt.Sprintf("base_url: %s → %s", orNone(haveBase), orNone(wantBase)))
		}
		if len(details) > 0 {
			p.add(backend.Change{Action: backend.Update, Kind: "connection", Name: name, Details: details, Run: func(ctx context.Context) error {
				return p.c.UpdateSecret(ctx, req)
			}})
		}
	}
}

func (p *planner) planModels() {
	for _, name := range p.s.ModelNames() {
		m := p.s.Models[name]
		provider := secretFor(p.s.Connections[m.Connection]).Provider
		req := func() ModelDefinitionRequest {
			return ModelDefinitionRequest{
				Name: name, SecretID: p.secretIDs[m.Connection], Provider: provider, ModelName: m.Model,
			}
		}

		have, exists := p.models[name]
		if !exists {
			p.add(backend.Change{Action: backend.Create, Kind: "model", Name: name, Details: []string{m.Connection + "/" + m.Model}, Run: func(ctx context.Context) error {
				r := req()
				r.CreatedBy = owner
				created, err := p.c.CreateModelDefinition(ctx, r)
				p.modelIDs[name] = created.ID
				return err
			}})
			continue
		}
		if !p.owned("model", name, have.CreatedBy) {
			continue
		}

		var details []string
		if existing, ok := p.secrets[m.Connection]; !ok || existing.ID != have.SecretID {
			details = append(details, fmt.Sprintf("connection: %s → %s", orNone(p.secretName[have.SecretID]), m.Connection))
		}
		if have.Provider != provider {
			details = append(details, fmt.Sprintf("provider: %s → %s", have.Provider, provider))
		}
		if have.ModelName != m.Model {
			details = append(details, fmt.Sprintf("model: %s → %s", have.ModelName, m.Model))
		}
		if len(details) > 0 {
			id := have.ID
			p.add(backend.Change{Action: backend.Update, Kind: "model", Name: name, Details: details, Run: func(ctx context.Context) error {
				r := req()
				r.ID, r.Name, r.UpdatedBy = id, "", owner
				return p.c.UpdateModelDefinition(ctx, r)
			}})
		}
	}
}

// link is a model attached to an endpoint, identified by model name.
type link struct {
	Model   string
	Linkage string
	Weight  float64
	Order   int
}

func desiredLinks(e spec.Endpoint) []link {
	var links []link
	weights := e.Weights()
	for i, t := range e.Route {
		links = append(links, link{Model: t.Model, Linkage: linkPrimary, Weight: float64(weights[i]) / 100})
	}
	for i, m := range e.Fallback {
		links = append(links, link{Model: m, Linkage: linkFallback, Weight: 1, Order: i + 1})
	}
	return links
}

func (p *planner) actualLinks(e Endpoint) []link {
	var links []link
	for _, m := range e.ModelMappings {
		l := link{Model: p.modelName[m.ModelDefinitionID], Linkage: m.LinkageType, Weight: m.Weight}
		if l.Model == "" {
			l.Model = m.ModelDefinitionID
		}
		if m.FallbackOrder != nil {
			l.Order = *m.FallbackOrder
		}
		links = append(links, l)
	}
	return links
}

// orderLike sorts links (which MLflow returns in no particular order) to follow
// the spec's order, so plan output reads as a diff.
func orderLike(links, want []link) {
	pos := map[string]int{}
	for i, l := range want {
		pos[l.Model] = i + 1
	}
	rank := func(l link) int {
		if p := pos[l.Model]; p > 0 {
			return p
		}
		return len(want) + 1
	}
	slices.SortStableFunc(links, func(a, b link) int {
		if a.Linkage != b.Linkage {
			return strings.Compare(b.Linkage, a.Linkage) // PRIMARY before FALLBACK
		}
		if a.Order != b.Order {
			return a.Order - b.Order
		}
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a.Model, b.Model)
	})
}

func linksEqual(a, b []link) bool {
	if len(a) != len(b) {
		return false
	}
	byModel := map[string]link{}
	for _, l := range b {
		byModel[l.Model] = l
	}
	for _, l := range a {
		o, ok := byModel[l.Model]
		if !ok || o.Linkage != l.Linkage || o.Order != l.Order || math.Abs(o.Weight-l.Weight) > 1e-4 {
			return false
		}
	}
	return true
}

func describeLinks(links []link) string {
	var route, fallback []string
	for _, l := range links {
		if l.Linkage == linkFallback {
			fallback = append(fallback, l.Model)
		} else {
			route = append(route, fmt.Sprintf("%s %.0f%%", l.Model, l.Weight*100))
		}
	}
	s := "[" + strings.Join(route, ", ") + "]"
	if len(fallback) > 0 {
		s += " fallback [" + strings.Join(fallback, ", ") + "]"
	}
	return s
}

func (p *planner) planEndpoints() {
	for _, name := range p.s.EndpointNames() {
		e := p.s.Endpoints[name]
		want := desiredLinks(e)
		req := func() EndpointRequest {
			r := EndpointRequest{UsageTracking: e.TrackUsage()}
			for _, l := range want {
				mc := ModelConfig{ModelDefinitionID: p.modelIDs[l.Model], LinkageType: l.Linkage, Weight: l.Weight}
				if l.Linkage == linkFallback {
					mc.FallbackOrder = &l.Order
				}
				r.ModelConfigs = append(r.ModelConfigs, mc)
			}
			if len(e.Route) > 1 {
				r.RoutingStrategy = trafficSplit
			}
			if len(e.Fallback) > 0 {
				r.FallbackConfig = &FallbackConfig{Strategy: sequential, MaxAttempts: len(e.Fallback)}
			}
			return r
		}

		have, exists := p.endpoints[name]
		if !exists {
			p.add(backend.Change{Action: backend.Create, Kind: "endpoint", Name: name, Details: []string{describeLinks(want)}, Run: func(ctx context.Context) error {
				r := req()
				r.Name, r.CreatedBy = name, owner
				return p.c.CreateEndpoint(ctx, r)
			}})
			continue
		}
		if !p.owned("endpoint", name, have.CreatedBy) {
			continue
		}

		var details []string
		got := p.actualLinks(have)
		orderLike(got, want)
		if !linksEqual(want, got) {
			details = append(details, fmt.Sprintf("%s → %s", describeLinks(got), describeLinks(want)))
		} else {
			// Same models; only routing settings can differ. Unused settings are
			// ignored because MLflow can't clear them and they have no effect.
			if len(e.Route) > 1 && have.RoutingStrategy != trafficSplit {
				details = append(details, "enable traffic split")
			}
			if fc := have.FallbackConfig; len(e.Fallback) > 0 && (fc == nil || fc.MaxAttempts != len(e.Fallback) || fc.Strategy != sequential) {
				details = append(details, "fallback settings")
			}
		}
		if have.UsageTracking != e.TrackUsage() {
			details = append(details, fmt.Sprintf("usage_tracking: %t → %t", have.UsageTracking, e.TrackUsage()))
		}
		if len(details) > 0 {
			id := have.ID
			p.add(backend.Change{Action: backend.Update, Kind: "endpoint", Name: name, Details: details, Run: func(ctx context.Context) error {
				r := req()
				r.ID, r.UpdatedBy = id, owner
				return p.c.UpdateEndpoint(ctx, r)
			}})
		}
	}
}

// planPrune handles kindling-created objects that were removed from the spec.
// Dependents go first: endpoints, then models, then connections.
func (p *planner) planPrune() {
	prune := func(kind, name, id string, del func(context.Context, string) error) {
		if !p.opts.Prune {
			p.plan.Unmanaged = append(p.plan.Unmanaged, kind+" "+name)
			return
		}
		p.add(backend.Change{Action: backend.Delete, Kind: kind, Name: name, Run: func(ctx context.Context) error {
			return del(ctx, id)
		}})
	}
	for _, name := range sortedNames(p.endpoints) {
		if x := p.endpoints[name]; x.CreatedBy == owner && !has(p.s.Endpoints, name) {
			prune("endpoint", name, x.ID, p.c.DeleteEndpoint)
		}
	}
	for _, name := range sortedNames(p.models) {
		if x := p.models[name]; x.CreatedBy == owner && !has(p.s.Models, name) {
			prune("model", name, x.ID, p.c.DeleteModelDefinition)
		}
	}
	for _, name := range sortedNames(p.secrets) {
		if x := p.secrets[name]; x.CreatedBy == owner && !has(p.s.Connections, name) {
			prune("connection", name, x.ID, p.c.DeleteSecret)
		}
	}
}

func masksMatch(masked, values map[string]string) bool {
	if len(masked) != len(values) {
		return false
	}
	for k, v := range values {
		if masked[k] != maskValue(v) {
			return false
		}
	}
	return true
}

func has[V any](m map[string]V, key string) bool {
	_, ok := m[key]
	return ok
}

func sortedNames[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

func trimSlash(s string) string { return strings.TrimRight(s, "/") }

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
