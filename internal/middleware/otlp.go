package middleware

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
)

// OTLPExporter sends spans as OTLP/HTTP JSON, e.g. to MLflow's /v1/traces.
type OTLPExporter struct {
	// URL is the full traces endpoint.
	URL  string
	HTTP *http.Client
	// Header, if set, returns extra headers for each export, such as the
	// MLflow experiment to store traces in.
	Header func(ctx context.Context) (http.Header, error)
	// Service is the resource's service.name.
	Service string
}

func (e *OTLPExporter) Export(ctx context.Context, spans []*Span) error {
	body, err := json.Marshal(otlpRequest(e.Service, spans))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if e.Header != nil {
		h, err := e.Header(ctx)
		if err != nil {
			return err
		}
		req.Header = h.Clone()
	}
	req.Header.Set("Content-Type", "application/json")
	client := e.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s returned HTTP %d: %s", e.URL, resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

// The OTLP JSON encoding: https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	String *string  `json:"stringValue,omitempty"`
	Bool   *bool    `json:"boolValue,omitempty"`
	Int    *string  `json:"intValue,omitempty"` // int64 as a JSON string
	Double *float64 `json:"doubleValue,omitempty"`
}

type otlpSpan struct {
	TraceID      string         `json:"traceId"`
	SpanID       string         `json:"spanId"`
	ParentSpanID string         `json:"parentSpanId,omitempty"`
	Name         string         `json:"name"`
	Kind         int            `json:"kind"`
	Start        string         `json:"startTimeUnixNano"`
	End          string         `json:"endTimeUnixNano"`
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
	Status       otlpStatus     `json:"status"`
}

type otlpStatus struct {
	Code    int    `json:"code"` // 1 OK, 2 ERROR
	Message string `json:"message,omitempty"`
}

const spanKindInternal = 1

func otlpRequest(service string, spans []*Span) map[string]any {
	var out []otlpSpan
	for _, s := range spans {
		o := otlpSpan{
			TraceID: s.TraceID(),
			SpanID:  hex.EncodeToString(s.ID[:]),
			Name:    s.Name,
			Kind:    spanKindInternal,
			Start:   strconv.FormatInt(s.StartTime.UnixNano(), 10),
			End:     strconv.FormatInt(s.EndTime.UnixNano(), 10),
			Status:  otlpStatus{Code: 1},
		}
		if s.ParentID != [8]byte{} {
			o.ParentSpanID = hex.EncodeToString(s.ParentID[:])
		}
		if s.Err != "" {
			o.Status = otlpStatus{Code: 2, Message: s.Err}
		}
		attrs := s.Attrs()
		for _, k := range slices.Sorted(maps.Keys(attrs)) {
			o.Attributes = append(o.Attributes, otlpKeyValue{Key: k, Value: otlpValueOf(attrs[k])})
		}
		out = append(out, o)
	}
	return map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []otlpKeyValue{
			{Key: "service.name", Value: otlpValueOf(service)},
		}},
		"scopeSpans": []any{map[string]any{
			"scope": map[string]string{"name": "kindling"},
			"spans": out,
		}},
	}}}
}

func otlpValueOf(v any) otlpValue {
	switch v := v.(type) {
	case string:
		return otlpValue{String: &v}
	case bool:
		return otlpValue{Bool: &v}
	case int:
		s := strconv.Itoa(v)
		return otlpValue{Int: &s}
	case int64:
		s := strconv.FormatInt(v, 10)
		return otlpValue{Int: &s}
	case float64:
		return otlpValue{Double: &v}
	default:
		s := fmt.Sprint(v)
		return otlpValue{String: &s}
	}
}
