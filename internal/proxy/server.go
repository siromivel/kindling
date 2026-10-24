// Package proxy is the edge's HTTP server: an OpenAI-compatible chat
// completions endpoint that runs each request through a middleware chain and
// forwards it to the router.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/siromivel/kindling/internal/middleware"
)

// maxRequestBytes bounds request bodies; images inline as data URLs are the
// big case.
const maxRequestBytes = 32 << 20

// TraceHeader carries the request's trace ID back to the client, to find the
// trace in MLflow.
const TraceHeader = "X-Kindling-Trace-Id"

type Config struct {
	// Upstream is the router's OpenAI-compatible base URL, e.g.
	// http://mlflow:5000/gateway/mlflow/v1.
	Upstream string
	// Chain returns the chain for a request's model.
	Chain func(model string) *middleware.Chain
	// Exporter receives each request's spans. Optional.
	Exporter middleware.Exporter
	Log      *slog.Logger
	HTTP     *http.Client
}

type Server struct {
	cfg Config
	mux *http.ServeMux
}

func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.HTTP == nil {
		// No overall timeout: streamed answers can take minutes. The client's
		// context bounds each request.
		cfg.HTTP = &http.Client{}
	}
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/chat/completions", s.chat)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Only chat completions pass through the middleware, so nothing else
		// is forwarded: that would bypass it.
		writeError(w, http.StatusNotFound, "invalid_request_error", "the kindling edge only serves POST /v1/chat/completions")
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// exchange is one request's state as it moves through the server.
type exchange struct {
	w     http.ResponseWriter
	req   *middleware.ChatRequest
	run   *middleware.Run
	trace *middleware.Trace
	root  *middleware.Span
	// upstream spans the router call, until its body is read.
	upstream *middleware.Span
	status   int
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body is too large")
		return
	}
	req, err := middleware.ParseChatRequest(body, r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	trace, root := middleware.NewTrace("chat.completions")
	root.Set("kindling.model", req.Model)
	root.Set("kindling.stream", req.Stream())
	x := &exchange{w: w, req: req, trace: trace, root: root}
	x.run = s.cfg.Chain(req.Model).Start(req, root, s.cfg.Log)
	defer s.finish(x)

	ctx := r.Context()
	resp, err := x.run.Request(ctx)
	if err != nil {
		s.stageFailed(x, err)
		return
	}
	if resp != nil {
		if req.Stream() && resp.StatusCode == http.StatusOK {
			s.replay(ctx, x, resp)
			return
		}
		s.respond(ctx, x, resp)
		return
	}

	upstream, err := s.forward(ctx, x)
	if err != nil {
		if ctx.Err() == nil {
			s.cfg.Log.Warn("calling the router", "error", err)
		}
		x.status = http.StatusBadGateway
		writeError(w, x.status, "upstream_error", "kindling couldn't reach the router")
		return
	}
	defer upstream.Body.Close()
	defer x.upstream.End()
	if req.Stream() && upstream.StatusCode == http.StatusOK && strings.HasPrefix(upstream.Header.Get("Content-Type"), "text/event-stream") {
		s.stream(ctx, x, upstream)
		return
	}
	data, err := io.ReadAll(upstream.Body)
	x.upstream.End()
	if err != nil {
		x.status = http.StatusBadGateway
		writeError(w, x.status, "upstream_error", "the router's response was cut off")
		return
	}
	s.respond(ctx, x, &middleware.ChatResponse{StatusCode: upstream.StatusCode, Header: upstream.Header, Body: data})
}

// forward sends the (possibly changed) request to the router.
func (s *Server) forward(ctx context.Context, x *exchange) (*http.Response, error) {
	span := x.root.Child("upstream")
	x.upstream = span
	body, err := x.req.MarshalJSON()
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Upstream+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range x.req.Header {
		if forwardable(k) {
			r.Header[k] = vs
		}
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.HTTP.Do(r)
	if err != nil {
		span.SetError(err)
		return nil, err
	}
	span.Set("http.status_code", resp.StatusCode)
	return resp, nil
}

// respond runs the response phase on a complete response and sends it.
func (s *Server) respond(ctx context.Context, x *exchange, resp *middleware.ChatResponse) {
	if err := x.run.Response(ctx, resp); err != nil {
		s.stageFailed(x, err)
		return
	}
	h := x.w.Header()
	for k, vs := range resp.Header {
		if forwardable(k) && k != "Content-Encoding" {
			h[k] = vs
		}
	}
	s.writeHeader(x, resp.StatusCode)
	x.w.Write(resp.Body)
}

// stream relays a streamed response chunk by chunk, flushing each one.
func (s *Server) stream(ctx context.Context, x *exchange, upstream *http.Response) {
	s.startStream(x)
	events := newSSEReader(upstream.Body)
	for {
		data, err := events.next()
		if errors.Is(err, io.EOF) || (err == nil && string(data) == "[DONE]") {
			break
		}
		if err != nil {
			// Cut off mid-stream; nothing is flushed, so nothing incomplete
			// gets stored.
			x.root.SetError(err)
			return
		}
		chunks, err := x.run.Chunk(ctx, &middleware.ChatChunk{Data: data})
		if err != nil {
			s.streamFailed(x, err)
			return
		}
		s.send(x, chunks)
	}
	x.upstream.End()
	s.endStream(ctx, x)
}

// replay answers a streamed request with a complete response, such as a
// cache hit, as a synthetic stream.
func (s *Server) replay(ctx context.Context, x *exchange, resp *middleware.ChatResponse) {
	var opts struct {
		IncludeUsage bool `json:"include_usage"`
	}
	x.req.Param("stream_options", &opts)
	chunks, err := middleware.ChunksFromResponse(resp.Body, opts.IncludeUsage)
	if err != nil {
		s.respond(ctx, x, resp)
		return
	}
	s.startStream(x)
	for _, c := range chunks {
		out, err := x.run.Chunk(ctx, c)
		if err != nil {
			s.streamFailed(x, err)
			return
		}
		s.send(x, out)
	}
	s.endStream(ctx, x)
}

func (s *Server) startStream(x *exchange) {
	h := x.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	s.writeHeader(x, http.StatusOK)
	flush(x.w)
}

func (s *Server) endStream(ctx context.Context, x *exchange) {
	chunks, err := x.run.Flush(ctx)
	if err != nil {
		s.streamFailed(x, err)
		return
	}
	s.send(x, chunks)
	writeEvent(x.w, []byte("[DONE]"))
	flush(x.w)
}

func (s *Server) send(x *exchange, chunks []*middleware.ChatChunk) {
	for _, c := range chunks {
		writeEvent(x.w, c.Data)
	}
	if len(chunks) > 0 {
		flush(x.w)
	}
}

func (s *Server) writeHeader(x *exchange, status int) {
	h := x.w.Header()
	for k, vs := range x.req.ResponseHeader {
		h[k] = vs
	}
	h.Set(TraceHeader, x.trace.TraceID())
	x.status = status
	x.w.WriteHeader(status)
}

// stageFailed answers with the OpenAI-shaped error for a fail_closed stage.
func (s *Server) stageFailed(x *exchange, err error) {
	x.w.Header().Set(TraceHeader, x.trace.TraceID())
	x.status = http.StatusServiceUnavailable
	writeError(x.w, x.status, "middleware_error", stageMessage(err))
}

// streamFailed ends a stream whose headers are already sent with an error
// event, and no [DONE].
func (s *Server) streamFailed(x *exchange, err error) {
	data, _ := json.Marshal(errorBody("middleware_error", stageMessage(err)))
	writeEvent(x.w, data)
	flush(x.w)
}

func stageMessage(err error) string {
	var se *middleware.StageError
	if errors.As(err, &se) {
		return "kindling middleware stage " + se.Stage + " failed, so the request was blocked"
	}
	return "kindling middleware failed, so the request was blocked"
}

func (s *Server) finish(x *exchange) {
	x.upstream.End()
	x.root.Set("http.status_code", x.status)
	x.root.End()
	spans := x.trace.Spans()
	var stages []string
	for _, sp := range spans {
		if o := sp.Outcome(); o != "" {
			stages = append(stages, sp.Name+"="+o)
		}
	}
	s.cfg.Log.Info("request", "model", x.req.Model, "stream", x.req.Stream(), "status", x.status,
		"stages", strings.Join(stages, " "), "trace_id", x.trace.TraceID())
	if s.cfg.Exporter != nil {
		s.cfg.Exporter.Export(context.Background(), spans)
	}
}

// forwardable reports whether a header passes through the edge: everything
// but hop-by-hop headers and ones the edge sets itself.
func forwardable(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade", "Host", "Content-Length", "Accept-Encoding":
		return false
	}
	return true
}

func flush(w http.ResponseWriter) {
	http.NewResponseController(w).Flush()
}

func errorBody(typ, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"message": msg, "type": typ, "param": nil, "code": nil}}
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody(typ, msg))
}
