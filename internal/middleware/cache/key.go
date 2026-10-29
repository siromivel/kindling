// Package cache is the cache stage: an exact-match cache of chat responses.
package cache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/siromivel/kindling/internal/middleware"
)

// ignoredParams don't change what an answer could be, so requests that differ
// only in them share cache entries. Notably, a streamed and a non-streamed
// request share one: either kind of response can answer the other.
var ignoredParams = []string{"stream", "stream_options", "user", "metadata", "store"}

// Key returns the cache key for a request: a SHA-256 over a canonical JSON
// encoding of the model, the messages (with whitespace normalized), every
// parameter that can change the answer (temperature, top_p, max_tokens, stop,
// seed, tools, tool_choice, response_format, ...), the endpoint's route
// fingerprint and the salt. Two requests get the same key exactly when the
// router would see the same question.
//
// The client's Authorization header is part of the key too, so a cache hit
// can never answer a client the router would have turned away.
func Key(req *middleware.ChatRequest, route, salt string) (string, error) {
	messages := make([]any, len(req.Messages))
	for i, m := range req.Messages {
		msg := map[string]any{"role": m.Role}
		for k, v := range m.Extra {
			c, err := canonical(v)
			if err != nil {
				return "", err
			}
			msg[k] = c
		}
		if m.Content != nil {
			c, err := canonicalContent(m.Content)
			if err != nil {
				return "", err
			}
			msg["content"] = c
		}
		messages[i] = msg
	}

	params := map[string]any{}
	for k, v := range req.Params {
		if slices.Contains(ignoredParams, k) {
			continue
		}
		c, err := canonical(v)
		if err != nil {
			return "", err
		}
		if k == "n" && c == int64(1) {
			continue // the default
		}
		params[k] = c
	}

	doc := map[string]any{
		"version":  1,
		"model":    req.Model,
		"messages": messages,
		"params":   params,
		"route":    route,
		"salt":     salt,
	}
	if auth := req.Header.Get("Authorization"); auth != "" {
		sum := sha256.Sum256([]byte(auth))
		doc["auth"] = hex.EncodeToString(sum[:])
	}
	data, err := json.Marshal(doc) // sorts map keys
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalContent is canonical, with the text of string content and of
// text parts whitespace-normalized.
func canonicalContent(raw json.RawMessage) (any, error) {
	v, err := canonical(raw)
	if err != nil {
		return nil, err
	}
	switch c := v.(type) {
	case string:
		return normalizeSpace(c), nil
	case []any:
		for _, p := range c {
			if part, ok := p.(map[string]any); ok && part["type"] == "text" {
				if text, ok := part["text"].(string); ok {
					part["text"] = normalizeSpace(text)
				}
			}
		}
	}
	return v, nil
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// canonical decodes JSON so that re-encoding it is canonical: object keys
// sorted (by encoding/json) and numbers in one form, so 0.70 and 0.7, or 1
// and 1.0, are the same.
func canonical(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return normalizeNumbers(v), nil
}

func normalizeNumbers(v any) any {
	switch v := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(v.String(), 10, 64); err == nil {
			return i
		}
		if f, err := v.Float64(); err == nil {
			if f == math.Trunc(f) && math.Abs(f) <= 1<<53 {
				return int64(f)
			}
			return f
		}
		return v.String()
	case []any:
		for i := range v {
			v[i] = normalizeNumbers(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = normalizeNumbers(v[k])
		}
	}
	return v
}
