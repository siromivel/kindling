package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/middleware/cache"
)

// fakeRouter is an OpenAI-compatible upstream. Streamed answers are sent as
// one chunk per word; with gate set, it waits on gate after the first chunk.
type fakeRouter struct {
	answer string
	status int
	gate   chan struct{}

	mu      sync.Mutex
	raw     [][]byte
	bodies  []map[string]any
	headers []http.Header
}

func (f *fakeRouter) start(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.raw = append(f.raw, raw)
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			for i, word := range strings.Fields(f.answer) {
				if i > 0 {
					word = " " + word
				}
				fmt.Fprintf(w, "data: {\"id\":\"s\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", word)
				w.(http.Flusher).Flush()
				if i == 0 && f.gate != nil {
					<-f.gate
				}
			}
			io.WriteString(w, "data: {\"id\":\"s\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "c", "object": "chat.completion",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": f.answer}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func (f *fakeRouter) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

type spanRecorder struct {
	mu    sync.Mutex
	spans [][]*middleware.Span
}

func (r *spanRecorder) Export(_ context.Context, spans []*middleware.Span) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, spans)
	return nil
}

func (r *spanRecorder) last() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.spans[len(r.spans)-1] {
		out = append(out, s.Name+"="+s.Outcome())
	}
	return out
}

func newServer(t *testing.T, upstream string, stages ...middleware.Stage) (*httptest.Server, *spanRecorder) {
	rec := &spanRecorder{}
	srv := httptest.NewServer(New(Config{
		Upstream: upstream,
		Chain:    func(string) *middleware.Chain { return &middleware.Chain{Stages: stages} },
		Exporter: rec,
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func post(t *testing.T, url, body string, header ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// streamed reads an SSE response and returns the assembled content and the
// number of data events.
func streamed(t *testing.T, resp *http.Response) (string, int) {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q: %s", ct, readBody(t, resp))
	}
	var content strings.Builder
	events, done := 0, false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		events++
		var c struct {
			Choices []struct {
				Delta struct{ Content string }
			}
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("bad chunk %q", data)
		}
		for _, ch := range c.Choices {
			content.WriteString(ch.Delta.Content)
		}
	}
	if !done {
		t.Error("stream didn't end with [DONE]")
	}
	return content.String(), events
}

const hello = `{"model":"chat","messages":[{"role":"user","content":"hi"}]}`

func TestPassthrough(t *testing.T) {
	router := &fakeRouter{answer: "hello there"}
	srv, rec := newServer(t, router.start(t))
	resp := post(t, srv.URL, `{"model":"chat","temperature":0.2,"messages":[{"role":"user","content":"hi"}],"x_vendor":{"a":1}}`,
		"Authorization", "Bearer k", "Connection", "close")
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, "hello there") {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	if len(resp.Header.Get(TraceHeader)) != 32 {
		t.Errorf("%s = %q", TraceHeader, resp.Header.Get(TraceHeader))
	}
	got := router.bodies[0]
	if got["temperature"] != 0.2 || got["model"] != "chat" || got["x_vendor"] == nil {
		t.Errorf("router got %v; unknown fields should pass through", got)
	}
	if h := router.headers[0]; h.Get("Authorization") != "Bearer k" || h.Get("Connection") == "close" {
		t.Errorf("forwarded headers %v", h)
	}
	if spans := rec.last(); !slices.Contains(spans, "upstream=") || !slices.Contains(spans, "chat.completions=") {
		t.Errorf("spans = %v", spans)
	}
}

// uppercase rewrites every message text, so the request is re-encoded with
// changed content.
type uppercase struct{ middleware.Base }

func (uppercase) Name() string                    { return "upper" }
func (uppercase) Phases() []middleware.Phase      { return []middleware.Phase{middleware.PhaseRequest} }
func (uppercase) OnError() middleware.ErrorPolicy { return middleware.FailClosed }
func (uppercase) OnRequest(_ context.Context, req *middleware.ChatRequest) (*middleware.ChatResponse, error) {
	for i := range req.Messages {
		texts, err := req.Messages[i].Texts()
		if err != nil {
			return nil, err
		}
		for j := range texts {
			texts[j] = strings.ToUpper(texts[j])
		}
		if err := req.Messages[i].SetTexts(texts); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// Fields the edge doesn't model must reach the router byte for byte: decoding
// them would lose precision (seed), number spelling (1.0, 2e3), escapes and
// key order, and dropping them would undo the cache key's handling of
// unknown params.
func TestUnknownFieldsReachRouterUnchanged(t *testing.T) {
	sent := `{"model":"chat","logprobs":true,"top_logprobs":3,"seed":12345678901234567890,` +
		`"x_vendor":{"z":1,"a":[1.0,2e3],"s":"\u00e9<\u003e"},` +
		`"messages":[{"role":"user","name":"al","x_meta":{"b":1,"a":2.50},` +
		`"content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`
	var want map[string]json.RawMessage
	json.Unmarshal([]byte(sent), &want)
	var wantMsgs []map[string]json.RawMessage
	json.Unmarshal(want["messages"], &wantMsgs)

	for name, stages := range map[string][]middleware.Stage{"no stages": nil, "text rewritten": {uppercase{}}} {
		t.Run(name, func(t *testing.T) {
			router := &fakeRouter{answer: "ok"}
			srv, _ := newServer(t, router.start(t), stages...)
			if resp := post(t, srv.URL, sent); resp.StatusCode != 200 {
				t.Fatalf("HTTP %d: %s", resp.StatusCode, readBody(t, resp))
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(router.raw[0], &got); err != nil {
				t.Fatalf("router got %s: %v", router.raw[0], err)
			}
			for _, k := range []string{"logprobs", "top_logprobs", "seed", "x_vendor"} {
				if string(got[k]) != string(want[k]) {
					t.Errorf("%s: router got %s, want %s", k, got[k], want[k])
				}
			}

			var msgs []map[string]json.RawMessage
			json.Unmarshal(got["messages"], &msgs)
			if len(msgs) != 1 {
				t.Fatalf("router got messages %s", got["messages"])
			}
			for _, k := range []string{"name", "x_meta"} {
				if string(msgs[0][k]) != string(wantMsgs[0][k]) {
					t.Errorf("message %s: router got %s, want %s", k, msgs[0][k], wantMsgs[0][k])
				}
			}
			var parts []map[string]json.RawMessage
			json.Unmarshal(msgs[0]["content"], &parts)
			if len(parts) != 1 || string(parts[0]["cache_control"]) != `{"type":"ephemeral","ttl":"5m"}` {
				t.Errorf("router got content %s", msgs[0]["content"])
			}
			wantText := `"hi"`
			if stages != nil {
				wantText = `"HI"`
			}
			if string(parts[0]["text"]) != wantText {
				t.Errorf("text = %s, want %s", parts[0]["text"], wantText)
			}
		})
	}
}

// A duplicated key could let one copy pass the edge's stages while the router
// reads the other, so the edge rejects the request before any stage runs.
func TestDuplicateKeysRejected(t *testing.T) {
	const pii = `[{"role":"user","content":"my SSN is 078-05-1120"}]`
	const clean = `[{"role":"user","content":"hi"}]`
	for name, body := range map[string]string{
		"messages, pii last":  `{"model":"chat","messages":` + clean + `,"messages":` + pii + `}`,
		"messages, pii first": `{"model":"chat","messages":` + pii + `,"messages":` + clean + `}`,
		"escaped key":         `{"model":"chat","messages":` + clean + `,"messag\u0065s":` + pii + `}`,
		"message content":     `{"model":"chat","messages":[{"role":"user","content":"hi","content":"my SSN is 078-05-1120"}]}`,
		"message role":        `{"model":"chat","messages":[{"role":"assistant","role":"user","content":"my SSN is 078-05-1120"}]}`,
		"content part text":   `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"hi","text":"my SSN is 078-05-1120"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			router := &fakeRouter{answer: "x"}
			srv, _ := newServer(t, router.start(t), uppercase{})
			resp := post(t, srv.URL, body)
			got := readBody(t, resp)
			if resp.StatusCode != 400 || !strings.Contains(got, "invalid_request_error") || !strings.Contains(got, "duplicate key") {
				t.Errorf("HTTP %d: %s", resp.StatusCode, got)
			}
			if strings.Contains(got, "078-05-1120") {
				t.Errorf("error echoes the request: %s", got)
			}
			if router.calls() != 0 {
				t.Errorf("router was called %d times", router.calls())
			}
		})
	}
}

func TestUpstreamErrorsPassThrough(t *testing.T) {
	router := &fakeRouter{status: http.StatusTooManyRequests}
	srv, _ := newServer(t, router.start(t))
	resp := post(t, srv.URL, hello)
	if body := readBody(t, resp); resp.StatusCode != 429 || !strings.Contains(body, "slow down") {
		t.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}

	dead, _ := newServer(t, "http://127.0.0.1:1/v1")
	resp = post(t, dead.URL, hello)
	if body := readBody(t, resp); resp.StatusCode != 502 || !strings.Contains(body, "upstream_error") {
		t.Errorf("unreachable router: HTTP %d: %s", resp.StatusCode, body)
	}
}

func TestBadRequests(t *testing.T) {
	srv, _ := newServer(t, "http://unused")
	resp := post(t, srv.URL, `not json`)
	if body := readBody(t, resp); resp.StatusCode != 400 || !strings.Contains(body, "invalid_request_error") {
		t.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	r, _ := http.Post(srv.URL+"/v1/embeddings", "application/json", strings.NewReader(`{}`))
	if r.StatusCode != 404 {
		t.Errorf("embeddings: HTTP %d, want 404 (it would bypass the middleware)", r.StatusCode)
	}
	r, _ = http.Get(srv.URL + "/healthz")
	if r.StatusCode != 200 {
		t.Errorf("healthz: HTTP %d", r.StatusCode)
	}
}

type failing struct {
	middleware.Base
	policy middleware.ErrorPolicy
}

func (failing) Name() string                      { return "pii" }
func (failing) Phases() []middleware.Phase        { return []middleware.Phase{middleware.PhaseRequest} }
func (f failing) OnError() middleware.ErrorPolicy { return f.policy }
func (failing) OnRequest(context.Context, *middleware.ChatRequest) (*middleware.ChatResponse, error) {
	return nil, errors.New("dial tcp 10.0.0.7:3000: connection refused")
}

func TestFailClosed(t *testing.T) {
	router := &fakeRouter{answer: "x"}
	srv, rec := newServer(t, router.start(t), failing{policy: middleware.FailClosed})
	for _, body := range []string{hello, strings.Replace(hello, `"model"`, `"stream":true,"model"`, 1)} {
		resp := post(t, srv.URL, body)
		var out struct {
			Error struct{ Message, Type string }
		}
		json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != 503 || out.Error.Type != "middleware_error" || !strings.Contains(out.Error.Message, "pii") {
			t.Errorf("HTTP %d, %+v", resp.StatusCode, out)
		}
		if strings.Contains(out.Error.Message, "10.0.0.7") {
			t.Errorf("error leaks internal detail: %q", out.Error.Message)
		}
	}
	if router.calls() != 0 {
		t.Errorf("router was called %d times; a fail_closed stage should block the request", router.calls())
	}
	if spans := rec.last(); !slices.Contains(spans, "pii.request=blocked") {
		t.Errorf("spans = %v", spans)
	}
}

func TestFailOpen(t *testing.T) {
	router := &fakeRouter{answer: "fine"}
	srv, rec := newServer(t, router.start(t), failing{policy: middleware.FailOpen})
	resp := post(t, srv.URL, hello)
	if body := readBody(t, resp); resp.StatusCode != 200 || !strings.Contains(body, "fine") {
		t.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	if spans := rec.last(); !slices.Contains(spans, "pii.request=error_failed_open") {
		t.Errorf("spans = %v", spans)
	}
}

func TestStreamingFlushesEachChunk(t *testing.T) {
	router := &fakeRouter{answer: "one two three", gate: make(chan struct{})}
	srv, _ := newServer(t, router.start(t), cache.New(cache.Config{TTL: time.Hour, Store: cache.NewLRU(10)}))
	// Cleanups run last-first: release the router before the servers close,
	// or a failure here would hang waiting on it.
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(router.gate) }) })

	// The router holds the rest of the stream until the client has the first
	// chunk, so this only finishes if the edge flushed it (and its headers)
	// right away.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	first := make(chan string, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(strings.Replace(hello, `"model"`, `"stream":true,"model"`, 1)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			first <- ""
			return
		}
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				first <- sc.Text()
				return
			}
		}
		first <- ""
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, `"one"`) {
			t.Errorf("first event = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first chunk didn't arrive while the router was still streaming; the edge is buffering")
	}
}

func TestStreamCacheReplay(t *testing.T) {
	router := &fakeRouter{answer: "one two three"}
	srv, rec := newServer(t, router.start(t), cache.New(cache.Config{TTL: time.Hour, Store: cache.NewLRU(10)}))
	stream := strings.Replace(hello, `"model"`, `"stream":true,"model"`, 1)

	resp := post(t, srv.URL, stream)
	if got, events := streamed(t, resp); got != "one two three" || events != 4 || resp.Header.Get(cache.Header) != "miss" {
		t.Fatalf("first stream: %q in %d events, %s = %q", got, events, cache.Header, resp.Header.Get(cache.Header))
	}
	if spans := rec.last(); !slices.Contains(spans, "cache.request=miss") || !slices.Contains(spans, "cache.response=stored") {
		t.Errorf("spans = %v", spans)
	}

	resp = post(t, srv.URL, stream)
	if got, events := streamed(t, resp); got != "one two three" || events < 2 || resp.Header.Get(cache.Header) != "hit" {
		t.Errorf("replay: %q in %d events, %s = %q", got, events, cache.Header, resp.Header.Get(cache.Header))
	}
	resp = post(t, srv.URL, hello)
	if body := readBody(t, resp); resp.Header.Get(cache.Header) != "hit" || !strings.Contains(body, `"content":"one two three"`) {
		t.Errorf("non-streamed request after a streamed one: %s = %q, %s", cache.Header, resp.Header.Get(cache.Header), body)
	}
	if router.calls() != 1 {
		t.Errorf("router called %d times, want 1", router.calls())
	}
}
