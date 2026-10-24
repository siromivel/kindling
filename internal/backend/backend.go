// Package backend defines how kindling drives a specific router.
package backend

import (
	"context"
	"fmt"
	"strings"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/spec"
)

// Backend reconciles a router's configuration with a kindling spec.
type Backend interface {
	// RequiredEnv lists settings the router's container needs for this spec.
	RequiredEnv(s *spec.Spec) []EnvRequirement
	// Plan compares the spec with the router's current state.
	Plan(ctx context.Context, s *spec.Spec, opts Options) (*Plan, error)
	// InferenceURL is the router's OpenAI-compatible base URL (ending in
	// /v1), where the edge forwards chat requests.
	InferenceURL(s *spec.Spec) string
	// TraceExporter returns where the edge sends its spans, or nil.
	TraceExporter(s *spec.Spec) middleware.Exporter
}

type Options struct {
	// Prune deletes kindling-managed objects that are no longer in the spec.
	Prune bool
	// RotateSecrets re-sends every credential even when it looks unchanged.
	RotateSecrets bool
}

// EnvRequirement is a container setting the router needs, e.g. to allow
// requests to private addresses.
type EnvRequirement struct {
	Key, Value, Reason string
}

type Action string

const (
	Create Action = "create"
	Update Action = "update"
	Delete Action = "delete"
)

// Change is one planned operation against the router.
type Change struct {
	Action  Action
	Kind    string // "connection", "model" or "endpoint"
	Name    string
	Details []string
	// Run performs the change. Changes must run in plan order, because later
	// changes refer to objects created by earlier ones.
	Run func(ctx context.Context) error
}

func (c Change) String() string {
	sym := map[Action]string{Create: "+", Update: "~", Delete: "-"}[c.Action]
	s := fmt.Sprintf("%s %s %s", sym, c.Kind, c.Name)
	if len(c.Details) > 0 {
		s += " (" + strings.Join(c.Details, "; ") + ")"
	}
	return s
}

// Plan is an ordered set of changes, plus notes for the user.
type Plan struct {
	Changes []Change
	// Unmanaged lists kindling-created objects missing from the spec that will
	// only be deleted with Options.Prune.
	Unmanaged []string
}

// Apply runs the plan's changes in order, stopping at the first failure.
func (p *Plan) Apply(ctx context.Context, progress func(Change)) error {
	for _, c := range p.Changes {
		if progress != nil {
			progress(c)
		}
		if err := c.Run(ctx); err != nil {
			return fmt.Errorf("%s %s %s: %w", c.Action, c.Kind, c.Name, err)
		}
	}
	return nil
}
