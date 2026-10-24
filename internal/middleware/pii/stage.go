package pii

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/spec"
)

// Outcome when at least one entity was replaced or masked.
const OutcomeRedacted = "redacted"

// concurrency bounds the analyzer calls in flight for one request.
const concurrency = 8

// Policy is a resolved pii policy: the roles to scan and a rule per entity.
type Policy struct {
	Name  string
	Roles []string
	Rules map[string]Rule
}

// PolicyFrom resolves a named policy from kindling.yaml.
func PolicyFrom(name string, p spec.PIIPolicy) Policy {
	rules := map[string]Rule{}
	for typ, e := range p.Entities {
		rules[typ] = Rule{Operator: e.Operator, Threshold: e.Threshold()}
	}
	return Policy{Name: name, Roles: p.Roles, Rules: rules}
}

// Stage redacts PII in request messages. Each instance applies one policy;
// endpoints with different policies get different instances.
type Stage struct {
	middleware.Base
	analyzer *Analyzer
	policy   Policy
	onError  middleware.ErrorPolicy

	entities []string
	minScore float64
}

func New(a *Analyzer, p Policy, onError middleware.ErrorPolicy) *Stage {
	s := &Stage{analyzer: a, policy: p, onError: onError, entities: slices.Sorted(maps.Keys(p.Rules)), minScore: 1}
	for _, r := range p.Rules {
		s.minScore = min(s.minScore, r.Threshold)
	}
	return s
}

func (s *Stage) Name() string                    { return spec.StagePII }
func (s *Stage) Phases() []middleware.Phase      { return []middleware.Phase{middleware.PhaseRequest} }
func (s *Stage) OnError() middleware.ErrorPolicy { return s.onError }

// OnRequest scans the content of messages in the policy's roles. Nothing is
// changed unless every text was analyzed, so a failure never forwards a
// partly redacted request.
func (s *Stage) OnRequest(ctx context.Context, req *middleware.ChatRequest) (*middleware.ChatResponse, error) {
	type job struct {
		msg, part int
		text      string
		result    string
		findings  []Finding
		err       error
	}
	var jobs []*job
	scanned := map[int][]string{}
	for i := range req.Messages {
		m := &req.Messages[i]
		if !slices.Contains(s.policy.Roles, m.Role) {
			continue
		}
		texts, err := m.Texts()
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		scanned[i] = texts
		for j, t := range texts {
			t = ValidUTF8(t) // the exact string both the analyzer and Redact see
			jobs = append(jobs, &job{msg: i, part: j, text: t, result: t})
		}
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for _, j := range jobs {
		if j.text == "" {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results, err := s.analyzer.Analyze(ctx, j.text, s.entities, s.minScore)
			if err != nil {
				j.err = err
				return
			}
			j.result, j.findings = Redact(j.text, results, s.policy.Rules)
		})
	}
	wg.Wait()

	counts := map[Finding]int{}
	changed := map[int]bool{}
	for _, j := range jobs {
		if j.err != nil {
			return nil, j.err
		}
		for _, f := range j.findings {
			counts[f]++
		}
		if j.result != j.text {
			scanned[j.msg][j.part] = j.result
			changed[j.msg] = true
		}
	}
	for i := range changed {
		if err := req.Messages[i].SetTexts(scanned[i]); err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
	}

	span := middleware.SpanFromContext(ctx)
	span.Set("kindling.pii.policy", s.policy.Name)
	redacted := 0
	for f, n := range counts {
		span.Set("kindling.pii."+f.Entity+".count", n)
		span.Set("kindling.pii."+f.Entity+".operator", f.Operator)
		if f.Operator != spec.OperatorKeep {
			redacted += n
		}
	}
	span.Set("kindling.pii.redacted", redacted)
	if redacted > 0 {
		span.SetOutcome(OutcomeRedacted)
	}
	return nil, nil
}
