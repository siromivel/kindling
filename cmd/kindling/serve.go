package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/middleware/cache"
	"github.com/siromivel/kindling/internal/middleware/pii"
	"github.com/siromivel/kindling/internal/proxy"
	"github.com/siromivel/kindling/internal/spec"
)

func runServe(ctx context.Context, args []string) error {
	var f specFlags
	var opts edgeOptions
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	f.register(fs)
	listen := fs.String("listen", "", "host:port to listen on (default: middleware.listen, or "+spec.DefaultEdgeListen+")")
	routerURL := fs.String("router-url", "", "the router's URL as seen from here (default: router.url)")
	fs.StringVar(&opts.presidioURL, "presidio-url", "", "Presidio analyzer URL (required with a pii stage)")
	parseFlags(fs, args)

	env, err := spec.ReadDotenv(f.envPath())
	if err != nil {
		return err
	}
	s, err := spec.LoadForEdge(f.file, env)
	if err != nil {
		return err
	}
	if s.Middleware == nil {
		return fmt.Errorf("%s has no middleware section, so there's nothing for the edge to do", f.file)
	}
	problems := s.ValidateMiddleware()
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	if spec.HasErrors(problems) {
		return errReported
	}
	if *routerURL != "" {
		s.Router.URL = strings.TrimRight(*routerURL, "/")
	}

	opts.log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	edge, err := newEdge(s, opts)
	if err != nil {
		return err
	}
	defer edge.close()

	addr := *listen
	if addr == "" {
		addr = s.Middleware.EffectiveListen()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: edge.handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	opts.log.Info("kindling edge listening", "addr", ln.Addr().String(), "upstream", edge.upstream)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type edgeOptions struct {
	presidioURL string
	// store replaces the cache stage's in-process store (for tests).
	store cache.Store
	log   *slog.Logger
}

type edge struct {
	handler  http.Handler
	upstream string
	close    func()
}

// newEdge builds the proxy and a chain per endpoint from the spec's
// middleware section.
func newEdge(s *spec.Spec, opts edgeOptions) (*edge, error) {
	b, err := newBackend(s)
	if err != nil {
		return nil, err
	}
	if opts.log == nil {
		opts.log = slog.Default()
	}
	m := s.Middleware
	e := &edge{upstream: m.Upstream, close: func() {}}
	if e.upstream == "" {
		e.upstream = b.InferenceURL(s)
	}

	// One pii stage per policy and one cache stage, shared by every chain
	// that uses them.
	piiStages := map[string]*pii.Stage{}
	var cacheStage *cache.Stage
	for _, st := range m.Stages {
		onError := middleware.FailClosed
		if st.OnError == spec.FailOpen {
			onError = middleware.FailOpen
		}
		switch st.Type {
		case spec.StagePII:
			if opts.presidioURL == "" {
				return nil, errors.New("the pii stage needs a Presidio analyzer: pass -presidio-url")
			}
			analyzer := pii.NewAnalyzer(strings.TrimRight(opts.presidioURL, "/"))
			for name, pol := range m.PIIPolicies {
				piiStages[name] = pii.New(analyzer, pii.PolicyFrom(name, pol), onError)
			}
		case spec.StageCache:
			routes := map[string]string{}
			for _, name := range s.EndpointNames() {
				routes[name] = s.RouteFingerprint(name)
			}
			store := opts.store
			if store == nil {
				store = cache.NewLRU(st.MaxEntries)
			}
			cacheStage = cache.New(cache.Config{OnError: onError, TTL: st.TTL, Salt: st.CacheSalt, Routes: routes, Store: store})
		}
	}
	chainFor := func(model string) *middleware.Chain {
		c := &middleware.Chain{}
		for _, es := range m.Chain(model) {
			if !es.Enabled {
				continue
			}
			switch es.Stage.Type {
			case spec.StagePII:
				c.Stages = append(c.Stages, piiStages[es.Policy])
			case spec.StageCache:
				c.Stages = append(c.Stages, cacheStage)
			}
		}
		return c
	}
	chains := map[string]*middleware.Chain{}
	for _, name := range s.EndpointNames() {
		chains[name] = chainFor(name)
	}
	others := chainFor("")

	var exporter middleware.Exporter
	if exp := b.TraceExporter(s); exp != nil {
		async := middleware.NewAsyncExporter(exp, opts.log)
		exporter, e.close = async, async.Close
	}
	e.handler = proxy.New(proxy.Config{
		Upstream: e.upstream,
		Chain: func(model string) *middleware.Chain {
			if c, ok := chains[model]; ok {
				return c
			}
			return others
		},
		Exporter: exporter,
		Log:      opts.log,
	})
	return e, nil
}

func runHealthcheck(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: kindling healthcheck <url>")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, args[0], nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", args[0], resp.StatusCode)
	}
	return nil
}
