package cache

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/siromivel/kindling/internal/middleware"
)

func request(t *testing.T, body string, header ...string) *middleware.ChatRequest {
	t.Helper()
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Add(header[i], header[i+1])
	}
	req, err := middleware.ParseChatRequest([]byte(body), h)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func key(t *testing.T, body string, header ...string) string {
	t.Helper()
	k, err := Key(request(t, body, header...), "route-1", "")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

const baseRequest = `{"model":"chat","temperature":0.7,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is 2+2?"}]}`

func TestKeyEquivalentRequests(t *testing.T) {
	want := key(t, baseRequest)
	for name, body := range map[string]string{
		"key order":   `{"messages":[{"content":"Be brief.","role":"system"},{"role":"user","content":"What is 2+2?"}],"temperature":0.7,"model":"chat"}`,
		"whitespace":  `{"model":"chat","temperature":0.7,"messages":[{"role":"system","content":"  Be   brief. "},{"role":"user","content":"What\nis 2+2?\t"}]}`,
		"number form": `{"model":"chat","temperature":0.70,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is 2+2?"}]}`,
		"streamed":    `{"model":"chat","stream":true,"stream_options":{"include_usage":true},"temperature":0.7,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is 2+2?"}]}`,
		"n: 1":        `{"model":"chat","n":1,"temperature":0.7,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is 2+2?"}]}`,
		"user id":     `{"model":"chat","user":"u-123","temperature":0.7,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"What is 2+2?"}]}`,
	} {
		if got := key(t, body); got != want {
			t.Errorf("%s: key differs from the base request's", name)
		}
	}
}

func TestKeyChangesWithAnything(t *testing.T) {
	base := key(t, baseRequest)
	seen := map[string]string{"base": base}
	for name, change := range map[string][2]string{
		"model":           {`"model":"chat"`, `"model":"other"`},
		"content":         {"What is 2+2?", "What is 2+3?"},
		"role":            {`"role":"system"`, `"role":"developer"`},
		"temperature":     {`"temperature":0.7`, `"temperature":0.8`},
		"top_p":           {`"temperature":0.7`, `"temperature":0.7,"top_p":0.9`},
		"max_tokens":      {`"temperature":0.7`, `"temperature":0.7,"max_tokens":10`},
		"stop":            {`"temperature":0.7`, `"temperature":0.7,"stop":["\n"]`},
		"seed":            {`"temperature":0.7`, `"temperature":0.7,"seed":42`},
		"tools":           {`"temperature":0.7`, `"temperature":0.7,"tools":[{"type":"function","function":{"name":"f"}}]`},
		"tool_choice":     {`"temperature":0.7`, `"temperature":0.7,"tool_choice":"none"`},
		"response_format": {`"temperature":0.7`, `"temperature":0.7,"response_format":{"type":"json_object"}`},
		"other params":    {`"temperature":0.7`, `"temperature":0.7,"logprobs":true`},
		"tool call":       {`{"role":"user","content":"What is 2+2?"}`, `{"role":"assistant","content":null,"tool_calls":[{"id":"a"}]}`},
		"image":           {`"content":"What is 2+2?"`, `"content":[{"type":"text","text":"What is 2+2?"},{"type":"image_url","image_url":{"url":"data:a"}}]`},
	} {
		if !strings.Contains(baseRequest, change[0]) {
			t.Fatalf("%s: base request lacks %q", name, change[0])
		}
		k := key(t, strings.Replace(baseRequest, change[0], change[1], 1))
		for other, ok := range seen {
			if ok == k {
				t.Errorf("changing %s gives the same key as %s", name, other)
			}
		}
		seen[name] = k
	}

	req := request(t, baseRequest)
	for name, k := range map[string]func() (string, error){
		"route": func() (string, error) { return Key(req, "route-2", "") },
		"salt":  func() (string, error) { return Key(req, "route-1", "2026-10") },
	} {
		if got, _ := k(); got == base {
			t.Errorf("changing the %s should change the key", name)
		}
	}
	if key(t, baseRequest, "Authorization", "Bearer a") == key(t, baseRequest, "Authorization", "Bearer b") {
		t.Error("different credentials should never share entries")
	}
}

func TestLRU(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(0, 0)
	c := NewLRU(2)
	c.now = func() time.Time { return now }
	c.Set(ctx, "a", []byte("1"), time.Minute)
	c.Set(ctx, "b", []byte("2"), time.Minute)
	c.Get(ctx, "a") // a is now more recent than b
	c.Set(ctx, "c", []byte("3"), time.Minute)
	if _, ok, _ := c.Get(ctx, "b"); ok {
		t.Error("b should have been evicted")
	}
	if v, ok, _ := c.Get(ctx, "a"); !ok || string(v) != "1" {
		t.Errorf("a = %q, %t", v, ok)
	}
	now = now.Add(time.Minute)
	if _, ok, _ := c.Get(ctx, "a"); ok {
		t.Error("a should have expired")
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d after expiry, want 1", c.Len())
	}
}

type brokenStore struct{}

func (brokenStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errors.New("store down")
}
func (brokenStore) Set(context.Context, string, []byte, time.Duration) error {
	return errors.New("store down")
}

const answer = `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"4"},"finish_reason":"stop"}]}`

// roundTrip runs a request through a chain with just the cache, answering
// misses with upstream.
func roundTrip(t *testing.T, stage *Stage, req *middleware.ChatRequest, upstream *middleware.ChatResponse) (*middleware.ChatResponse, string) {
	t.Helper()
	run := (&middleware.Chain{Stages: []middleware.Stage{stage}}).Start(req, nil, nil)
	resp, err := run.Request(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		resp = upstream
	}
	if err := run.Response(context.Background(), resp); err != nil {
		t.Fatal(err)
	}
	return resp, req.ResponseHeader.Get(Header)
}

func newStage(store Store) *Stage {
	return New(Config{OnError: middleware.FailOpen, TTL: time.Hour, Store: store, Routes: map[string]string{"chat": "r1"}})
}

func TestStageHitMiss(t *testing.T) {
	stage := newStage(NewLRU(10))
	ok := &middleware.ChatResponse{StatusCode: 200, Body: []byte(answer)}

	if _, h := roundTrip(t, stage, request(t, baseRequest), ok); h != "miss" {
		t.Errorf("first request: %s = %q, want miss", Header, h)
	}
	resp, h := roundTrip(t, stage, request(t, baseRequest), nil)
	if h != "hit" || string(resp.Body) != answer {
		t.Errorf("second request: %s = %q, body %s", Header, h, resp.Body)
	}
}

func TestStageDoesntStore(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		headers []string
		resp    *middleware.ChatResponse
		header  string
	}{
		"n > 1":               {strings.Replace(baseRequest, `"model"`, `"n":2,"model"`, 1), nil, &middleware.ChatResponse{StatusCode: 200, Body: []byte(answer)}, "bypass"},
		"no-cache":            {baseRequest, []string{"Cache-Control", "no-cache"}, &middleware.ChatResponse{StatusCode: 200, Body: []byte(answer)}, "bypass"},
		"error":               {baseRequest, nil, &middleware.ChatResponse{StatusCode: 500, Body: []byte(`{"error":{"message":"down"}}`)}, "miss"},
		"200 with error body": {baseRequest, nil, &middleware.ChatResponse{StatusCode: 200, Body: []byte(`{"error":{"message":"down"}}`)}, "miss"},
		"cut off":             {baseRequest, nil, &middleware.ChatResponse{StatusCode: 200, Body: []byte(`{"choices":[{"index":0,"message":{"content":"4"}}]}`)}, "miss"},
	} {
		t.Run(name, func(t *testing.T) {
			store := NewLRU(10)
			stage := newStage(store)
			if _, h := roundTrip(t, stage, request(t, tc.body, tc.headers...), tc.resp); h != tc.header {
				t.Errorf("%s = %q, want %q", Header, h, tc.header)
			}
			if store.Len() != 0 {
				t.Error("response was stored")
			}
		})
	}
}

func TestStageFailOpen(t *testing.T) {
	stage := newStage(brokenStore{})
	resp, h := roundTrip(t, stage, request(t, baseRequest), &middleware.ChatResponse{StatusCode: 200, Body: []byte(answer)})
	if string(resp.Body) != answer || h != "" {
		t.Errorf("with a broken store: body %s, header %q; want the upstream answer and no header", resp.Body, h)
	}
}

func TestStageStream(t *testing.T) {
	store := NewLRU(10)
	stage := newStage(store)
	req := request(t, strings.Replace(baseRequest, `"model"`, `"stream":true,"model"`, 1))
	run := (&middleware.Chain{Stages: []middleware.Stage{stage}}).Start(req, nil, nil)
	if resp, err := run.Request(context.Background()); resp != nil || err != nil {
		t.Fatal(resp, err)
	}
	for _, data := range []string{
		`{"id":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"4"},"finish_reason":null}]}`,
		`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	} {
		out, err := run.Chunk(context.Background(), &middleware.ChatChunk{Data: []byte(data)})
		if err != nil || len(out) != 1 || string(out[0].Data) != data {
			t.Fatalf("Chunk should pass through unchanged: %v, %v", out, err)
		}
	}
	if _, err := run.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A non-streamed request for the same thing is answered from the stream.
	resp, h := roundTrip(t, stage, request(t, baseRequest), nil)
	if h != "hit" || !strings.Contains(string(resp.Body), `"content":"4"`) {
		t.Errorf("%s = %q, body %s", Header, h, resp.Body)
	}
}
