package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/spec"
)

// Header tells clients whether the cache answered: hit, miss or bypass.
const Header = "X-Kindling-Cache"

// Outcomes recorded on the stage's spans.
const (
	OutcomeHit    = "hit"
	OutcomeMiss   = "miss"
	OutcomeBypass = "bypass"
	OutcomeStored = "stored"
)

type Config struct {
	OnError middleware.ErrorPolicy
	TTL     time.Duration
	Salt    string
	// Routes maps endpoint names to their spec.RouteFingerprint. Other models
	// have no fingerprint.
	Routes map[string]string
	Store  Store
}

// Stage answers repeated requests from the store and stores new answers.
type Stage struct {
	middleware.Base
	cfg Config
}

func New(cfg Config) *Stage { return &Stage{cfg: cfg} }

func (s *Stage) Name() string { return spec.StageCache }
func (s *Stage) Phases() []middleware.Phase {
	return []middleware.Phase{middleware.PhaseRequest, middleware.PhaseResponse}
}
func (s *Stage) OnError() middleware.ErrorPolicy { return s.cfg.OnError }

// pending is a miss waiting for its response.
type pending struct {
	key string
	asm middleware.Assembler
}

func (s *Stage) OnRequest(ctx context.Context, req *middleware.ChatRequest) (*middleware.ChatResponse, error) {
	span := middleware.SpanFromContext(ctx)
	if reason := bypass(req); reason != "" {
		req.ResponseHeader.Set(Header, OutcomeBypass)
		span.SetOutcome(OutcomeBypass)
		span.Set("kindling.cache.bypass", reason)
		return nil, nil
	}
	key, err := Key(req, s.cfg.Routes[req.Model], s.cfg.Salt)
	if err != nil {
		return nil, fmt.Errorf("computing cache key: %w", err)
	}
	body, ok, err := s.cfg.Store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("reading the cache: %w", err)
	}
	if ok {
		req.ResponseHeader.Set(Header, OutcomeHit)
		span.SetOutcome(OutcomeHit)
		return &middleware.ChatResponse{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       body,
		}, nil
	}
	req.SetValue(s, &pending{key: key})
	req.ResponseHeader.Set(Header, OutcomeMiss)
	span.SetOutcome(OutcomeMiss)
	return nil, nil
}

// bypass returns why a request shouldn't be cached at all, or "".
func bypass(req *middleware.ChatRequest) string {
	var n int
	if req.Param("n", &n) && n > 1 {
		return "n > 1"
	}
	for _, v := range req.Header.Values("Cache-Control") {
		for _, d := range strings.Split(v, ",") {
			if d := strings.ToLower(strings.TrimSpace(d)); d == "no-cache" || d == "no-store" {
				return "cache-control: " + d
			}
		}
	}
	return ""
}

func (s *Stage) OnResponse(ctx context.Context, req *middleware.ChatRequest, resp *middleware.ChatResponse) error {
	p, _ := req.Value(s).(*pending)
	if p == nil {
		return nil
	}
	if resp.StatusCode != http.StatusOK || !complete(resp.Body) {
		middleware.SpanFromContext(ctx).Set("kindling.cache.not_stored", fmt.Sprintf("HTTP %d or incomplete response", resp.StatusCode))
		return nil
	}
	return s.store(ctx, p.key, resp.Body)
}

func (s *Stage) OnChunk(_ context.Context, req *middleware.ChatRequest, c *middleware.ChatChunk) ([]*middleware.ChatChunk, error) {
	if p, _ := req.Value(s).(*pending); p != nil {
		p.asm.Add(c)
	}
	return []*middleware.ChatChunk{c}, nil
}

func (s *Stage) Flush(ctx context.Context, req *middleware.ChatRequest) ([]*middleware.ChatChunk, error) {
	p, _ := req.Value(s).(*pending)
	if p == nil {
		return nil, nil
	}
	body, ok := p.asm.Body()
	if !ok {
		middleware.SpanFromContext(ctx).Set("kindling.cache.not_stored", "incomplete stream")
		return nil, nil
	}
	return nil, s.store(ctx, p.key, body)
}

func (s *Stage) store(ctx context.Context, key string, body []byte) error {
	if err := s.cfg.Store.Set(ctx, key, body, s.cfg.TTL); err != nil {
		return fmt.Errorf("writing the cache: %w", err)
	}
	middleware.SpanFromContext(ctx).SetOutcome(OutcomeStored)
	return nil
}

// complete reports whether body is a finished answer worth storing: every
// choice has a finish_reason, and it isn't an error.
func complete(body []byte) bool {
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Error != nil || len(resp.Choices) == 0 {
		return false
	}
	for _, c := range resp.Choices {
		if c.FinishReason == "" {
			return false
		}
	}
	return true
}
