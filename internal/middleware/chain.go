package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
)

// Span outcomes the chain records on its own. Stages set more specific ones
// (hit, miss, redacted, ...) with SpanFromContext(ctx).SetOutcome.
const (
	OutcomePass            = "pass"
	OutcomeBlocked         = "blocked"
	OutcomeErrorFailedOpen = "error_failed_open"
)

// Chain is the ordered list of stages a request runs through. Requests pass
// through it in order, responses in reverse (the onion model).
type Chain struct {
	Stages []Stage
}

// StageError is a fail_closed stage failing. Clients get a 503 that names the
// stage, without the underlying error.
type StageError struct {
	Stage string
	Phase Phase
	Err   error
}

func (e *StageError) Error() string {
	return fmt.Sprintf("stage %s failed in the %s phase: %v", e.Stage, e.Phase, e.Err)
}

func (e *StageError) Unwrap() error { return e.Err }

// Run is one request's pass through a chain. It isn't safe for concurrent use.
type Run struct {
	stages []Stage
	req    *ChatRequest
	parent *Span
	log    *slog.Logger

	// respond is how many stages, from the start of the chain, run the
	// response phase: all of them, or only those before a stage that
	// answered the request itself.
	respond int
	// skipped holds stages that failed open and sit out the rest of the run.
	skipped map[int]bool
	// streamSpans are the response-phase spans of a stream, which last from
	// the first chunk to Flush.
	streamSpans map[int]*Span
}

// Start begins a run of req through the chain. Stage spans are children of
// parent (which may be nil), and fail-open errors are logged to log.
func (c *Chain) Start(req *ChatRequest, parent *Span, log *slog.Logger) *Run {
	if log == nil {
		log = slog.Default()
	}
	var stages []Stage
	if c != nil {
		stages = c.Stages
	}
	return &Run{
		stages: stages, req: req, parent: parent, log: log,
		respond: len(stages), skipped: map[int]bool{}, streamSpans: map[int]*Span{},
	}
}

// Request runs the request phase in chain order. A non-nil response means a
// stage answered the request, and it shouldn't be sent to the router. The
// only errors are *StageError, from stages that fail closed.
func (r *Run) Request(ctx context.Context) (*ChatResponse, error) {
	for i, st := range r.stages {
		if !r.runs(i, PhaseRequest) {
			continue
		}
		var resp *ChatResponse
		span := r.startSpan(i, PhaseRequest)
		err := r.invoke(ctx, i, PhaseRequest, span, func(ctx context.Context) (err error) {
			resp, err = st.OnRequest(ctx, r.req)
			return err
		})
		endSpan(span)
		if err != nil {
			return nil, err
		}
		if resp != nil {
			r.respond = i
			return resp, nil
		}
	}
	return nil, nil
}

// Response runs the response phase on a complete response, in reverse chain
// order.
func (r *Run) Response(ctx context.Context, resp *ChatResponse) error {
	for i := r.respond - 1; i >= 0; i-- {
		if !r.runs(i, PhaseResponse) {
			continue
		}
		st := r.stages[i]
		span := r.startSpan(i, PhaseResponse)
		err := r.invoke(ctx, i, PhaseResponse, span, func(ctx context.Context) error {
			return st.OnResponse(ctx, r.req, resp)
		})
		endSpan(span)
		if err != nil {
			return err
		}
	}
	return nil
}

// Chunk passes one streamed chunk through the response phase, in reverse
// chain order, and returns the chunks to send to the client now.
func (r *Run) Chunk(ctx context.Context, chunk *ChatChunk) ([]*ChatChunk, error) {
	return r.passChunks(ctx, r.respond-1, []*ChatChunk{chunk})
}

// Flush ends a stream. Each stage, innermost first, emits what it buffered,
// and those chunks pass through the stages outside it.
func (r *Run) Flush(ctx context.Context) ([]*ChatChunk, error) {
	defer r.endStream()
	var pending []*ChatChunk
	for i := r.respond - 1; i >= 0; i-- {
		if !r.runs(i, PhaseResponse) {
			continue
		}
		var err error
		if pending, err = r.chunkThrough(ctx, i, pending); err != nil {
			return nil, err
		}
		if r.skipped[i] {
			continue
		}
		st := r.stages[i]
		var out []*ChatChunk
		err = r.invoke(ctx, i, PhaseResponse, r.streamSpan(i), func(ctx context.Context) (err error) {
			out, err = st.Flush(ctx, r.req)
			return err
		})
		if err != nil {
			return nil, err
		}
		pending = append(pending, out...)
	}
	return pending, nil
}

// passChunks runs chunks through stages from..0.
func (r *Run) passChunks(ctx context.Context, from int, chunks []*ChatChunk) ([]*ChatChunk, error) {
	for i := from; i >= 0; i-- {
		var err error
		if chunks, err = r.chunkThrough(ctx, i, chunks); err != nil {
			return nil, err
		}
	}
	return chunks, nil
}

// chunkThrough runs chunks through stage i's OnChunk.
func (r *Run) chunkThrough(ctx context.Context, i int, chunks []*ChatChunk) ([]*ChatChunk, error) {
	if !r.runs(i, PhaseResponse) || len(chunks) == 0 {
		return chunks, nil
	}
	st := r.stages[i]
	var next []*ChatChunk
	for _, c := range chunks {
		if r.skipped[i] {
			// Failed open on an earlier chunk: the rest go past it unchanged.
			next = append(next, c)
			continue
		}
		var out []*ChatChunk
		err := r.invoke(ctx, i, PhaseResponse, r.streamSpan(i), func(ctx context.Context) (err error) {
			out, err = st.OnChunk(ctx, r.req, c)
			return err
		})
		if err != nil {
			r.endStream()
			return nil, err
		}
		if r.skipped[i] {
			out = []*ChatChunk{c}
		}
		next = append(next, out...)
	}
	return next, nil
}

func (r *Run) runs(i int, p Phase) bool {
	return !r.skipped[i] && slices.Contains(r.stages[i].Phases(), p)
}

func (r *Run) startSpan(i int, p Phase) *Span {
	span := r.parent.Child(r.stages[i].Name() + "." + p.String())
	span.Set("kindling.stage", r.stages[i].Name())
	span.Set("kindling.phase", p.String())
	return span
}

func (r *Run) streamSpan(i int) *Span {
	if s, ok := r.streamSpans[i]; ok {
		return s
	}
	s := r.startSpan(i, PhaseResponse)
	r.streamSpans[i] = s
	return s
}

func (r *Run) endStream() {
	for _, s := range r.streamSpans {
		endSpan(s)
	}
	clear(r.streamSpans)
}

// endSpan ends a stage span, recording "pass" if the stage set no outcome.
func endSpan(s *Span) {
	if s.Outcome() == "" {
		s.SetOutcome(OutcomePass)
	}
	s.End()
}

// invoke calls one stage hook and applies its error policy. It returns an
// error only when the stage fails closed.
func (r *Run) invoke(ctx context.Context, i int, p Phase, span *Span, hook func(context.Context) error) error {
	st := r.stages[i]
	err := safely(ContextWithSpan(ctx, span), hook)
	if err == nil {
		return nil
	}
	span.SetError(err)
	if st.OnError() == FailClosed {
		span.SetOutcome(OutcomeBlocked)
		return &StageError{Stage: st.Name(), Phase: p, Err: err}
	}
	span.SetOutcome(OutcomeErrorFailedOpen)
	r.skipped[i] = true
	r.log.Warn("stage failed open; skipping it for this request", "stage", st.Name(), "phase", p.String(), "error", err)
	return nil
}

// safely turns a panicking stage into an error, so its error policy applies.
func safely(ctx context.Context, hook func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return hook(ctx)
}
