// Package middleware runs OpenAI chat requests through a chain of stages
// (PII redaction, caching, ...) on their way to the router and back.
package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
)

// ChatRequest is an OpenAI chat completions request. Model and Messages are
// decoded for stages to read and change; every other field stays raw in
// Params and is forwarded unchanged.
type ChatRequest struct {
	Model    string
	Messages []Message
	Params   map[string]json.RawMessage

	// Header is the client's request headers.
	Header http.Header
	// ResponseHeader collects headers stages add to the client's response.
	ResponseHeader http.Header

	values map[any]any
}

// ParseChatRequest decodes a chat completions request body.
func ParseChatRequest(body []byte, header http.Header) (*ChatRequest, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	r := &ChatRequest{Params: fields, Header: header, ResponseHeader: http.Header{}}
	if r.Header == nil {
		r.Header = http.Header{}
	}
	if raw, ok := fields["model"]; ok {
		if err := json.Unmarshal(raw, &r.Model); err != nil {
			return nil, errors.New("model must be a string")
		}
		delete(fields, "model")
	}
	if raw, ok := fields["messages"]; ok {
		if err := json.Unmarshal(raw, &r.Messages); err != nil {
			return nil, fmt.Errorf("messages must be an array of messages: %w", err)
		}
		delete(fields, "messages")
	}
	return r, nil
}

func (r *ChatRequest) MarshalJSON() ([]byte, error) {
	out := maps.Clone(r.Params)
	if out == nil {
		out = map[string]json.RawMessage{}
	}
	var err error
	if out["model"], err = marshal(r.Model); err != nil {
		return nil, err
	}
	if out["messages"], err = marshal(r.Messages); err != nil {
		return nil, err
	}
	return marshal(out)
}

// Param decodes the named request parameter into v, reporting whether it was
// present and of the right type.
func (r *ChatRequest) Param(name string, v any) bool {
	raw, ok := r.Params[name]
	return ok && json.Unmarshal(raw, v) == nil
}

// Stream reports whether the client asked for a streamed response.
func (r *ChatRequest) Stream() bool {
	var stream bool
	return r.Param("stream", &stream) && stream
}

// Value returns per-request state a stage stored with SetValue. Stages are
// shared between requests, so this is where they keep anything that spans
// the request and response phases.
func (r *ChatRequest) Value(key any) any { return r.values[key] }

func (r *ChatRequest) SetValue(key, v any) {
	if r.values == nil {
		r.values = map[any]any{}
	}
	r.values[key] = v
}

// Message is one chat message. Content is kept raw because it's either a
// string or an array of content parts; Texts and SetTexts handle both.
type Message struct {
	Role    string
	Content json.RawMessage
	// Extra holds every other field (name, tool_calls, tool_call_id, ...).
	Extra map[string]json.RawMessage
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return errors.New("message must be an object")
	}
	if raw, ok := fields["role"]; ok {
		if err := json.Unmarshal(raw, &m.Role); err != nil {
			return errors.New("message role must be a string")
		}
	}
	m.Content = fields["content"]
	delete(fields, "role")
	delete(fields, "content")
	m.Extra = fields
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	out := maps.Clone(m.Extra)
	if out == nil {
		out = map[string]json.RawMessage{}
	}
	out["role"], _ = marshal(m.Role)
	if m.Content != nil {
		out["content"] = m.Content
	}
	return marshal(out)
}

// Texts returns the message's text: its content string, or the text of each
// text part of multi-part content. Other parts (images, audio, files) are
// skipped.
func (m *Message) Texts() ([]string, error) {
	var texts []string
	_, err := m.rewriteTexts(func(s string) string {
		texts = append(texts, s)
		return s
	})
	return texts, err
}

// SetTexts replaces the message's text, in the order Texts returned it.
func (m *Message) SetTexts(texts []string) error {
	i := 0
	content, err := m.rewriteTexts(func(s string) string {
		if i < len(texts) {
			s = texts[i]
		}
		i++
		return s
	})
	if err == nil {
		m.Content = content
	}
	return err
}

// rewriteTexts returns the content with each text passed through fn.
func (m *Message) rewriteTexts(fn func(string) string) (json.RawMessage, error) {
	if !isSet(m.Content) {
		return m.Content, nil
	}
	content := bytes.TrimSpace(m.Content)
	switch content[0] {
	case '"':
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return nil, err
		}
		return marshal(fn(s))
	case '[':
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(content, &parts); err != nil {
			return nil, errors.New("message content parts must be objects")
		}
		for _, part := range parts {
			var typ, text string
			if json.Unmarshal(part["type"], &typ) != nil || typ != "text" {
				continue
			}
			if err := json.Unmarshal(part["text"], &text); err != nil {
				return nil, errors.New("text content part needs a string text field")
			}
			part["text"], _ = marshal(fn(text))
		}
		return marshal(parts)
	default:
		return nil, errors.New("message content must be a string or an array of content parts")
	}
}

// isSet reports whether raw holds a JSON value other than null.
func isSet(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && string(raw) != "null"
}

// marshal is json.Marshal without HTML escaping, so "<EMAIL_ADDRESS>" reaches
// the router as written.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ChatResponse is a complete (non-streamed) response, from the router or
// from a stage that answered the request itself.
type ChatResponse struct {
	StatusCode int
	Header     http.Header
	// Body is the JSON response body, forwarded to the client as-is.
	Body []byte
}

// ChatChunk is one event of a streamed response: the JSON payload of an SSE
// "data:" line. The stream's closing [DONE] marker isn't a chunk.
type ChatChunk struct {
	Data []byte
}
