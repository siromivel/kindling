package middleware

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// Assembler rebuilds a complete chat completion from the chunks of a stream,
// so a streamed answer can be stored like any other.
type Assembler struct {
	id, model, fingerprint string
	created                json.RawMessage
	usage                  json.RawMessage
	choices                map[int]*assembledChoice
	// broken is set by anything that can't be reassembled faithfully.
	broken bool
}

type assembledChoice struct {
	role      string
	text      map[string]*strings.Builder // content, refusal, reasoning_content, ...
	other     map[string]json.RawMessage
	toolCalls map[int]*assembledToolCall
	finish    string
}

type assembledToolCall struct {
	id, typ, name string
	args          strings.Builder
}

type chunkChoice struct {
	Index        int                        `json:"index"`
	Delta        map[string]json.RawMessage `json:"delta"`
	FinishReason *string                    `json:"finish_reason"`
	Logprobs     json.RawMessage            `json:"logprobs"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Add records one chunk.
func (a *Assembler) Add(c *ChatChunk) {
	var chunk struct {
		ID                string          `json:"id"`
		Created           json.RawMessage `json:"created"`
		Model             string          `json:"model"`
		SystemFingerprint string          `json:"system_fingerprint"`
		Choices           []chunkChoice   `json:"choices"`
		Usage             json.RawMessage `json:"usage"`
		Error             json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(c.Data, &chunk); err != nil || chunk.Error != nil {
		a.broken = true
		return
	}
	if a.choices == nil {
		a.choices = map[int]*assembledChoice{}
	}
	a.id = cmp.Or(a.id, chunk.ID)
	a.model = cmp.Or(a.model, chunk.Model)
	a.fingerprint = cmp.Or(a.fingerprint, chunk.SystemFingerprint)
	if a.created == nil {
		a.created = chunk.Created
	}
	if isSet(chunk.Usage) {
		a.usage = chunk.Usage
	}
	for _, cc := range chunk.Choices {
		if isSet(cc.Logprobs) {
			// Not reassembled: a replay would silently drop them.
			a.broken = true
		}
		ch := a.choices[cc.Index]
		if ch == nil {
			ch = &assembledChoice{text: map[string]*strings.Builder{}, other: map[string]json.RawMessage{}, toolCalls: map[int]*assembledToolCall{}}
			a.choices[cc.Index] = ch
		}
		if cc.FinishReason != nil {
			ch.finish = *cc.FinishReason
		}
		for k, v := range cc.Delta {
			if !isSet(v) {
				continue
			}
			switch k {
			case "role":
				json.Unmarshal(v, &ch.role)
			case "tool_calls":
				var calls []toolCallDelta
				if json.Unmarshal(v, &calls) != nil {
					a.broken = true
				}
				for _, d := range calls {
					tc := ch.toolCalls[d.Index]
					if tc == nil {
						tc = &assembledToolCall{}
						ch.toolCalls[d.Index] = tc
					}
					tc.id = cmp.Or(tc.id, d.ID)
					tc.typ = cmp.Or(tc.typ, d.Type)
					tc.name += d.Function.Name
					tc.args.WriteString(d.Function.Arguments)
				}
			default:
				var s string
				if json.Unmarshal(v, &s) == nil {
					if ch.text[k] == nil {
						ch.text[k] = &strings.Builder{}
					}
					ch.text[k].WriteString(s)
				} else {
					ch.other[k] = v
				}
			}
		}
	}
}

// Body returns the assembled chat completion, or false if the stream didn't
// finish cleanly (some choice has no finish_reason) or couldn't be parsed.
func (a *Assembler) Body() ([]byte, bool) {
	if a.broken || len(a.choices) == 0 {
		return nil, false
	}
	var choices []map[string]any
	for _, i := range slices.Sorted(maps.Keys(a.choices)) {
		ch := a.choices[i]
		if ch.finish == "" {
			return nil, false
		}
		msg := map[string]any{"role": cmp.Or(ch.role, "assistant"), "content": nil}
		for k, v := range ch.other {
			msg[k] = v
		}
		for k, b := range ch.text {
			msg[k] = b.String()
		}
		if len(ch.toolCalls) > 0 {
			var calls []map[string]any
			for _, j := range slices.Sorted(maps.Keys(ch.toolCalls)) {
				tc := ch.toolCalls[j]
				calls = append(calls, map[string]any{
					"id": tc.id, "type": cmp.Or(tc.typ, "function"),
					"function": map[string]string{"name": tc.name, "arguments": tc.args.String()},
				})
			}
			msg["tool_calls"] = calls
		}
		choices = append(choices, map[string]any{"index": i, "message": msg, "finish_reason": ch.finish, "logprobs": nil})
	}
	out := map[string]any{"id": a.id, "object": "chat.completion", "model": a.model, "choices": choices}
	if a.created != nil {
		out["created"] = a.created
	}
	if a.fingerprint != "" {
		out["system_fingerprint"] = a.fingerprint
	}
	if a.usage != nil {
		out["usage"] = a.usage
	}
	body, err := marshal(out)
	return body, err == nil
}

// ChunksFromResponse turns a complete chat completion into the chunks a
// stream of it would have had, for answering a streamed request from a
// stored response. With includeUsage (stream_options.include_usage), a final
// chunk carries the usage.
func ChunksFromResponse(body []byte, includeUsage bool) ([]*ChatChunk, error) {
	var resp struct {
		ID                string          `json:"id"`
		Created           json.RawMessage `json:"created"`
		Model             string          `json:"model"`
		SystemFingerprint string          `json:"system_fingerprint,omitempty"`
		Choices           []struct {
			Index        int                        `json:"index"`
			Message      map[string]json.RawMessage `json:"message"`
			FinishReason json.RawMessage            `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var chunks []*ChatChunk
	emit := func(choices []map[string]any, usage json.RawMessage) error {
		c := map[string]any{"id": resp.ID, "object": "chat.completion.chunk", "created": resp.Created, "model": resp.Model, "choices": choices}
		if resp.SystemFingerprint != "" {
			c["system_fingerprint"] = resp.SystemFingerprint
		}
		if usage != nil {
			c["usage"] = usage
		}
		data, err := marshal(c)
		chunks = append(chunks, &ChatChunk{Data: data})
		return err
	}
	for _, ch := range resp.Choices {
		delta := map[string]any{}
		for k, v := range ch.Message {
			if !isSet(v) {
				continue
			}
			if k == "tool_calls" {
				var calls []map[string]any
				if err := json.Unmarshal(v, &calls); err != nil {
					return nil, err
				}
				for j, call := range calls {
					call["index"] = j
				}
				delta[k] = calls
				continue
			}
			delta[k] = v
		}
		if err := emit([]map[string]any{{"index": ch.Index, "delta": delta, "finish_reason": nil}}, nil); err != nil {
			return nil, err
		}
		if err := emit([]map[string]any{{"index": ch.Index, "delta": map[string]any{}, "finish_reason": ch.FinishReason}}, nil); err != nil {
			return nil, err
		}
	}
	if includeUsage && isSet(resp.Usage) {
		if err := emit([]map[string]any{}, resp.Usage); err != nil {
			return nil, err
		}
	}
	return chunks, nil
}
