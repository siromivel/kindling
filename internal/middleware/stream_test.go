package middleware

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func chunks(data ...string) []*ChatChunk {
	var out []*ChatChunk
	for _, d := range data {
		out = append(out, &ChatChunk{Data: []byte(d)})
	}
	return out
}

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	json.Unmarshal([]byte(want), &w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestAssembler(t *testing.T) {
	var a Assembler
	for _, c := range chunks(
		`{"id":"c1","object":"chat.completion.chunk","created":7,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"c1","created":7,"model":"m","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{"id":"c1","created":7,"model":"m","choices":[{"index":0,"delta":{"content":"lo","reasoning_content":"hm"},"finish_reason":null}]}`,
		`{"id":"c1","created":7,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"get","arguments":"{\"a\""}}]},"finish_reason":null}]}`,
		`{"id":"c1","created":7,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","created":7,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c1","created":7,"model":"m","choices":[],"usage":{"total_tokens":3}}`,
	) {
		a.Add(c)
	}
	body, ok := a.Body()
	if !ok {
		t.Fatal("stream should be complete")
	}
	jsonEqual(t, body, `{"id":"c1","object":"chat.completion","created":7,"model":"m",
		"choices":[{"index":0,"finish_reason":"stop","logprobs":null,"message":{"role":"assistant","content":"Hello","reasoning_content":"hm",
			"tool_calls":[{"id":"t1","type":"function","function":{"name":"get","arguments":"{\"a\":1}"}}]}}],
		"usage":{"total_tokens":3}}`)
}

func TestAssemblerIncomplete(t *testing.T) {
	for name, data := range map[string][]string{
		"no finish_reason": {`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`},
		"error event":      {`{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"stop"}]}`, `{"error":{"message":"boom"}}`},
		"logprobs":         {`{"choices":[{"index":0,"delta":{"content":"x"},"logprobs":{"content":[]},"finish_reason":"stop"}]}`},
		"not json":         {`nope`},
		"nothing":          {},
	} {
		var a Assembler
		for _, c := range chunks(data...) {
			a.Add(c)
		}
		if _, ok := a.Body(); ok {
			t.Errorf("%s: Body() ok, want not cacheable", name)
		}
	}
}

func TestChunksFromResponseRoundTrip(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","created":7,"model":"m",
		"choices":[{"index":0,"finish_reason":"tool_calls","logprobs":null,"message":{"role":"assistant","content":"Hi",
			"tool_calls":[{"id":"t1","type":"function","function":{"name":"get","arguments":"{}"}}]}}],
		"usage":{"total_tokens":3}}`
	out, err := ChunksFromResponse([]byte(body), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d chunks, want content, finish and usage", len(out))
	}
	if !strings.Contains(string(out[0].Data), `"object":"chat.completion.chunk"`) || !strings.Contains(string(out[0].Data), `"index":0`) {
		t.Errorf("first chunk = %s", out[0].Data)
	}
	var a Assembler
	for _, c := range out {
		a.Add(c)
	}
	got, ok := a.Body()
	if !ok {
		t.Fatal("replayed stream should reassemble")
	}
	jsonEqual(t, got, body)

	noUsage, _ := ChunksFromResponse([]byte(body), false)
	if len(noUsage) != 2 {
		t.Errorf("without include_usage got %d chunks, want 2", len(noUsage))
	}
}

func TestChatRequestRoundTrip(t *testing.T) {
	body := `{"model":"chat","temperature":0.2,"messages":[
		{"role":"system","content":"Be brief"},
		{"role":"user","name":"al","content":[{"type":"text","text":"Mail a@b.co"},{"type":"image_url","image_url":{"url":"data:x"}},{"type":"text","text":"thanks"}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"t"}]}],"stream":true}`
	req, err := ParseChatRequest([]byte(body), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "chat" || !req.Stream() || len(req.Messages) != 3 {
		t.Fatalf("parsed %+v", req)
	}
	texts, err := req.Messages[1].Texts()
	if err != nil || len(texts) != 2 || texts[0] != "Mail a@b.co" || texts[1] != "thanks" {
		t.Fatalf("Texts = %q, %v", texts, err)
	}
	if err := req.Messages[1].SetTexts([]string{"Mail <EMAIL_ADDRESS>", "thanks"}); err != nil {
		t.Fatal(err)
	}
	if texts, _ := req.Messages[2].Texts(); texts != nil {
		t.Errorf("null content texts = %q", texts)
	}
	out, err := req.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out, strings.Replace(body, "Mail a@b.co", "Mail <EMAIL_ADDRESS>", 1))
	if strings.Contains(string(out), "\\u003c") {
		t.Errorf("HTML-escaped output: %s", out)
	}

	for _, bad := range []string{`[]`, `{"model":1}`, `{"messages":{}}`, `{"messages":[1]}`, `{"model":"a"} {}`,
		`{"model":"a","model":"b"}`, `{"messages":[{"role":"user","role":"system"}]}`,
		`{"messages":[{"content":[{"type":"text","text":"a","t\u0065xt":"b"}]}]}`} {
		if _, err := ParseChatRequest([]byte(bad), nil); err == nil {
			t.Errorf("ParseChatRequest(%s) should fail", bad)
		}
	}
	m := Message{Role: "user", Content: json.RawMessage(`42`)}
	if _, err := m.Texts(); err == nil {
		t.Error("numeric content should be an error")
	}
}
