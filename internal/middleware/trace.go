package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"maps"
	"sync"
	"time"
)

// Trace collects the spans of one request: a root span, one span per stage
// per phase, and one for the router call.
type Trace struct {
	ID [16]byte

	mu    sync.Mutex
	spans []*Span
}

// Span is a timed step of a request. Its methods are safe to call on a nil
// *Span, so stages can annotate SpanFromContext(ctx) without checking.
type Span struct {
	ID        [8]byte
	ParentID  [8]byte // zero for the root
	Name      string
	StartTime time.Time
	EndTime   time.Time
	// Err is the error message, if the step failed. Stage errors must never
	// contain request content.
	Err string

	mu    sync.Mutex
	attrs map[string]any // string, bool, int or float64
	trace *Trace
}

// NewTrace starts a trace and its root span.
func NewTrace(name string) (*Trace, *Span) {
	t := &Trace{}
	rand.Read(t.ID[:])
	return t, t.start(name, [8]byte{})
}

// TraceID returns the trace ID in hex, as OTLP and W3C trace context write it.
func (t *Trace) TraceID() string { return hex.EncodeToString(t.ID[:]) }

// Spans returns the trace's spans, in start order.
func (t *Trace) Spans() []*Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*Span(nil), t.spans...)
}

func (t *Trace) start(name string, parent [8]byte) *Span {
	s := &Span{ParentID: parent, Name: name, StartTime: time.Now(), attrs: map[string]any{}, trace: t}
	rand.Read(s.ID[:])
	t.mu.Lock()
	t.spans = append(t.spans, s)
	t.mu.Unlock()
	return s
}

// Child starts a span under s.
func (s *Span) Child(name string) *Span {
	if s == nil {
		return nil
	}
	return s.trace.start(name, s.ID)
}

// Set records an attribute. Values should be strings, bools, ints or floats.
func (s *Span) Set(key string, v any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.attrs[key] = v
	s.mu.Unlock()
}

// SetOutcome records what a stage did: pass, hit, miss, redacted, ...
func (s *Span) SetOutcome(outcome string) { s.Set("kindling.outcome", outcome) }

// Outcome returns the recorded outcome, or "".
func (s *Span) Outcome() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := s.attrs["kindling.outcome"].(string)
	return v
}

// TraceID returns the span's trace ID in hex.
func (s *Span) TraceID() string { return s.trace.TraceID() }

// SetError marks the span as failed.
func (s *Span) SetError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.Err = err.Error()
	s.mu.Unlock()
}

// Attrs returns a copy of the span's attributes.
func (s *Span) Attrs() map[string]any {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.attrs)
}

// End finishes the span and records its duration. Only the first call counts.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.EndTime.IsZero() {
		s.EndTime = time.Now()
		s.attrs["kindling.duration_ms"] = float64(s.EndTime.Sub(s.StartTime).Microseconds()) / 1000
	}
}

type spanKey struct{}

// ContextWithSpan returns ctx carrying span, for SpanFromContext.
func ContextWithSpan(ctx context.Context, span *Span) context.Context {
	return context.WithValue(ctx, spanKey{}, span)
}

// SpanFromContext returns the current stage's span, or nil.
func SpanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}

// Exporter sends a finished trace's spans somewhere.
type Exporter interface {
	Export(ctx context.Context, spans []*Span) error
}

// AsyncExporter exports in the background so slow trace storage never delays
// responses. When its queue is full, traces are dropped (and logged).
type AsyncExporter struct {
	exp   Exporter
	log   *slog.Logger
	queue chan []*Span
	done  chan struct{}
}

func NewAsyncExporter(exp Exporter, log *slog.Logger) *AsyncExporter {
	a := &AsyncExporter{exp: exp, log: log, queue: make(chan []*Span, 1024), done: make(chan struct{})}
	go func() {
		defer close(a.done)
		for spans := range a.queue {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := a.exp.Export(ctx, spans); err != nil {
				a.log.Warn("exporting trace", "trace_id", spans[0].TraceID(), "error", err)
			}
			cancel()
		}
	}()
	return a
}

func (a *AsyncExporter) Export(_ context.Context, spans []*Span) error {
	if len(spans) == 0 {
		return nil
	}
	select {
	case a.queue <- spans:
	default:
		a.log.Warn("trace export queue is full; dropping trace", "trace_id", spans[0].TraceID())
	}
	return nil
}

// Close exports what's queued and stops.
func (a *AsyncExporter) Close() {
	close(a.queue)
	<-a.done
}
