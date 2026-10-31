package middleware

import "context"

type Phase int

const (
	PhaseRequest Phase = iota
	PhaseResponse
)

func (p Phase) String() string {
	if p == PhaseRequest {
		return "request"
	}
	return "response"
}

// ErrorPolicy says what happens to a request when a stage fails.
type ErrorPolicy int

const (
	// FailClosed rejects the request with a 503.
	FailClosed ErrorPolicy = iota
	// FailOpen logs the error, skips the stage and carries on.
	FailOpen
)

// Stage is one step of the chain. Embed Base to implement only the hooks a
// stage needs.
type Stage interface {
	Name() string
	Phases() []Phase
	OnError() ErrorPolicy

	// OnRequest may change the request, or answer it by returning a complete
	// response (e.g. a cache hit), which skips the rest of the request phase
	// and the router.
	OnRequest(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

	// OnResponse sees a complete response.
	OnResponse(ctx context.Context, req *ChatRequest, resp *ChatResponse) error

	// OnChunk sees each chunk of a streamed response and returns the chunks
	// to pass on. Returning none means "buffered, emit later": stages that
	// need context across chunks keep their own window. Flush is called at
	// the end of the stream.
	OnChunk(ctx context.Context, req *ChatRequest, chunk *ChatChunk) ([]*ChatChunk, error)
	Flush(ctx context.Context, req *ChatRequest) ([]*ChatChunk, error)
}

// Base implements Stage's hooks as no-ops that pass everything through.
type Base struct{}

func (Base) OnRequest(context.Context, *ChatRequest) (*ChatResponse, error) { return nil, nil }

func (Base) OnResponse(context.Context, *ChatRequest, *ChatResponse) error { return nil }

func (Base) OnChunk(_ context.Context, _ *ChatRequest, c *ChatChunk) ([]*ChatChunk, error) {
	return []*ChatChunk{c}, nil
}

func (Base) Flush(context.Context, *ChatRequest) ([]*ChatChunk, error) { return nil, nil }
