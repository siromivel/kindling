package middleware

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// recorder is a stage that logs every hook call, and can be told to fail,
// answer requests itself, or buffer chunks until Flush.
type recorder struct {
	Base
	name     string
	log      *[]string
	policy   ErrorPolicy
	phases   []Phase
	fail     map[string]bool // hook name → return an error
	answer   bool
	buffer   bool
	buffered []*ChatChunk
}

func (r *recorder) Name() string         { return r.name }
func (r *recorder) OnError() ErrorPolicy { return r.policy }
func (r *recorder) Phases() []Phase {
	if r.phases != nil {
		return r.phases
	}
	return []Phase{PhaseRequest, PhaseResponse}
}

func (r *recorder) hook(name string) error {
	*r.log = append(*r.log, r.name+"."+name)
	if r.fail[name] {
		return errors.New(r.name + " broke")
	}
	return nil
}

func (r *recorder) OnRequest(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	if err := r.hook("request"); err != nil {
		return nil, err
	}
	if r.answer {
		SpanFromContext(ctx).SetOutcome("hit")
		return &ChatResponse{StatusCode: 200, Body: []byte(`{"from":"` + r.name + `"}`)}, nil
	}
	req.Model += "+" + r.name
	return nil, nil
}

func (r *recorder) OnResponse(ctx context.Context, req *ChatRequest, resp *ChatResponse) error {
	if err := r.hook("response"); err != nil {
		return err
	}
	resp.Body = append(resp.Body, " "+r.name...)
	return nil
}

func (r *recorder) OnChunk(ctx context.Context, req *ChatRequest, c *ChatChunk) ([]*ChatChunk, error) {
	if err := r.hook("chunk"); err != nil {
		return nil, err
	}
	out := &ChatChunk{Data: append(slices.Clone(c.Data), r.name...)}
	if r.buffer {
		r.buffered = append(r.buffered, out)
		return nil, nil
	}
	return []*ChatChunk{out}, nil
}

func (r *recorder) Flush(ctx context.Context, req *ChatRequest) ([]*ChatChunk, error) {
	if err := r.hook("flush"); err != nil {
		return nil, err
	}
	out := r.buffered
	r.buffered = nil
	return out, nil
}

func newChain(log *[]string, stages ...*recorder) *Chain {
	c := &Chain{}
	for _, s := range stages {
		s.log = log
		c.Stages = append(c.Stages, s)
	}
	return c
}

func chunkData(chunks []*ChatChunk) []string {
	var out []string
	for _, c := range chunks {
		out = append(out, string(c.Data))
	}
	return out
}

func assertLog(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestChainOrder(t *testing.T) {
	var log []string
	chain := newChain(&log, &recorder{name: "a"}, &recorder{name: "b"}, &recorder{name: "c"})
	req := &ChatRequest{Model: "m"}
	run := chain.Start(req, nil, nil)

	resp, err := run.Request(context.Background())
	if err != nil || resp != nil {
		t.Fatalf("Request = %v, %v", resp, err)
	}
	if req.Model != "m+a+b+c" {
		t.Errorf("request phase order: model = %q", req.Model)
	}
	resp = &ChatResponse{Body: []byte("upstream")}
	if err := run.Response(context.Background(), resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "upstream c b a" {
		t.Errorf("response phase should run in reverse: body = %q", resp.Body)
	}
	assertLog(t, log, []string{"a.request", "b.request", "c.request", "c.response", "b.response", "a.response"})
}

func TestChainShortCircuit(t *testing.T) {
	var log []string
	chain := newChain(&log, &recorder{name: "a"}, &recorder{name: "cache", answer: true}, &recorder{name: "c"})
	run := chain.Start(&ChatRequest{}, nil, nil)
	resp, err := run.Request(context.Background())
	if err != nil || resp == nil {
		t.Fatalf("Request = %v, %v; want the cache's answer", resp, err)
	}
	if err := run.Response(context.Background(), resp); err != nil {
		t.Fatal(err)
	}
	// c never sees the request; the stages before the cache see the answer.
	assertLog(t, log, []string{"a.request", "cache.request", "a.response"})
	if string(resp.Body) != `{"from":"cache"} a` {
		t.Errorf("body = %q", resp.Body)
	}
}

func TestChainResponseOnlyStage(t *testing.T) {
	var log []string
	chain := newChain(&log, &recorder{name: "out", phases: []Phase{PhaseResponse}}, &recorder{name: "in"})
	run := chain.Start(&ChatRequest{}, nil, nil)
	run.Request(context.Background())
	run.Response(context.Background(), &ChatResponse{})
	assertLog(t, log, []string{"in.request", "in.response", "out.response"})
}

func TestChainFailClosed(t *testing.T) {
	for _, hook := range []string{"request", "response"} {
		t.Run(hook, func(t *testing.T) {
			var log []string
			chain := newChain(&log,
				&recorder{name: "pii", policy: FailClosed, fail: map[string]bool{hook: true}},
				&recorder{name: "b"})
			_, root := NewTrace("test")
			run := chain.Start(&ChatRequest{}, root, nil)
			_, err := run.Request(context.Background())
			if err == nil {
				err = run.Response(context.Background(), &ChatResponse{})
			}
			var se *StageError
			if !errors.As(err, &se) || se.Stage != "pii" || se.Phase.String() != hook {
				t.Fatalf("err = %v, want a StageError from pii's %s phase", err, hook)
			}
			if hook == "request" {
				assertLog(t, log, []string{"pii.request"})
			}
			span := findSpan(t, root, "pii."+hook)
			if span.Outcome() != OutcomeBlocked || span.Err != "pii broke" {
				t.Errorf("span outcome %q err %q", span.Outcome(), span.Err)
			}
		})
	}
}

func TestChainFailOpen(t *testing.T) {
	var log []string
	chain := newChain(&log,
		&recorder{name: "cache", policy: FailOpen, fail: map[string]bool{"request": true}},
		&recorder{name: "b"})
	_, root := NewTrace("test")
	req := &ChatRequest{Model: "m"}
	run := chain.Start(req, root, nil)
	resp, err := run.Request(context.Background())
	if err != nil || resp != nil {
		t.Fatalf("Request = %v, %v", resp, err)
	}
	if err := run.Response(context.Background(), &ChatResponse{}); err != nil {
		t.Fatal(err)
	}
	// The failed stage sits out the rest of the request, including its
	// response phase.
	assertLog(t, log, []string{"cache.request", "b.request", "b.response"})
	if req.Model != "m+b" {
		t.Errorf("model = %q", req.Model)
	}
	if got := findSpan(t, root, "cache.request").Outcome(); got != OutcomeErrorFailedOpen {
		t.Errorf("outcome = %q", got)
	}
	if got := findSpan(t, root, "b.request").Outcome(); got != OutcomePass {
		t.Errorf("default outcome = %q, want pass", got)
	}
}

type panicky struct{ Base }

func (panicky) Name() string         { return "panicky" }
func (panicky) Phases() []Phase      { return []Phase{PhaseRequest} }
func (panicky) OnError() ErrorPolicy { return FailClosed }
func (panicky) OnRequest(context.Context, *ChatRequest) (*ChatResponse, error) {
	panic("oops")
}

func TestChainPanicIsAnError(t *testing.T) {
	run := (&Chain{Stages: []Stage{panicky{}}}).Start(&ChatRequest{}, nil, nil)
	if _, err := run.Request(context.Background()); err == nil || !strings.Contains(err.Error(), "panic: oops") {
		t.Errorf("err = %v", err)
	}
}

func TestChainStreaming(t *testing.T) {
	var log []string
	buffering := &recorder{name: "b", buffer: true}
	chain := newChain(&log, &recorder{name: "a"}, buffering, &recorder{name: "c"})
	_, root := NewTrace("test")
	run := chain.Start(&ChatRequest{}, root, nil)
	run.Request(context.Background())

	var sent []string
	for _, data := range []string{"1", "2"} {
		out, err := run.Chunk(context.Background(), &ChatChunk{Data: []byte(data)})
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, chunkData(out)...)
	}
	if len(sent) != 0 {
		t.Errorf("b buffers, so nothing should be sent before Flush; sent %v", sent)
	}
	out, err := run.Flush(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// c (innermost) saw each chunk first, then b, whose flushed chunks pass
	// out through a exactly once.
	if got, want := chunkData(out), []string{"1cba", "2cba"}; !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v", got, want)
	}
	assertLog(t, log, []string{
		"a.request", "b.request", "c.request",
		"c.chunk", "b.chunk", "c.chunk", "b.chunk",
		"c.flush", "b.flush", "a.chunk", "a.chunk", "a.flush",
	})
	// One response span per stage covers the whole stream.
	if n := countSpans(root, "b.response"); n != 1 {
		t.Errorf("b.response spans = %d, want 1", n)
	}
	if findSpan(t, root, "b.response").EndTime.IsZero() {
		t.Error("stream spans should end at Flush")
	}
}

func TestChainStreamFailOpenPassesChunksThrough(t *testing.T) {
	var log []string
	chain := newChain(&log, &recorder{name: "a", policy: FailOpen, fail: map[string]bool{"chunk": true}})
	run := chain.Start(&ChatRequest{}, nil, nil)
	run.Request(context.Background())
	var sent []string
	for _, data := range []string{"1", "2"} {
		out, err := run.Chunk(context.Background(), &ChatChunk{Data: []byte(data)})
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, chunkData(out)...)
	}
	out, _ := run.Flush(context.Background())
	sent = append(sent, chunkData(out)...)
	if !slices.Equal(sent, []string{"1", "2"}) {
		t.Errorf("sent %v, want the original chunks", sent)
	}
	assertLog(t, log, []string{"a.request", "a.chunk"})
}

func TestChainStreamFailClosed(t *testing.T) {
	var log []string
	chain := newChain(&log, &recorder{name: "a", policy: FailClosed, fail: map[string]bool{"chunk": true}})
	run := chain.Start(&ChatRequest{}, nil, nil)
	run.Request(context.Background())
	var se *StageError
	if _, err := run.Chunk(context.Background(), &ChatChunk{Data: []byte("1")}); !errors.As(err, &se) {
		t.Errorf("err = %v, want StageError", err)
	}
}

func findSpan(t *testing.T, root *Span, name string) *Span {
	t.Helper()
	for _, s := range root.trace.Spans() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %s span", name)
	return nil
}

func countSpans(root *Span, name string) int {
	n := 0
	for _, s := range root.trace.Spans() {
		if s.Name == name {
			n++
		}
	}
	return n
}
