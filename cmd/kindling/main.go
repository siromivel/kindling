// Command kindling sets up and configures self-hosted LLM routers.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/siromivel/kindling/internal/backend"
	"github.com/siromivel/kindling/internal/mlflow"
	"github.com/siromivel/kindling/internal/spec"
	mlflowrouter "github.com/siromivel/kindling/routers/mlflow"
)

const usage = `kindling sets up and configures self-hosted LLM routers.

Usage:
  kindling init      write docker-compose.yaml, .env and a starter kindling.yaml
  kindling validate  check kindling.yaml
  kindling plan      show what apply would change on the router
  kindling apply     make the router match kindling.yaml
  kindling serve     run the middleware proxy in front of the router

Run "kindling <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "init":
		err = runInit(args)
	case "validate":
		err = runValidate(args)
	case "plan":
		err = runPlan(ctx, args, false)
	case "apply":
		err = runPlan(ctx, args, true)
	case "serve":
		err = runServe(ctx, args)
	case "healthcheck":
		// For the edge container's healthcheck, which has no shell or curl.
		err = runHealthcheck(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "kindling: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if errors.Is(err, errChanges) {
		os.Exit(2)
	}
	if err != nil {
		if !errors.Is(err, errReported) {
			fmt.Fprintln(os.Stderr, "kindling:", err)
		}
		os.Exit(1)
	}
}

// errReported means the problem was already printed.
var errReported = errors.New("reported")

// errChanges is `plan -detailed-exitcode` reporting that the router doesn't
// match kindling.yaml. It exits 2, like terraform plan -detailed-exitcode.
var errChanges = errors.New("changes pending")

// parseFlags behaves like flag.ExitOnError but exits 1 on bad flags, not 2, so
// they can't be mistaken for errChanges.
func parseFlags(fs *flag.FlagSet, args []string) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(1)
	}
}

type specFlags struct {
	file, envFile string
}

func (f *specFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.file, "f", "kindling.yaml", "path to kindling.yaml")
	fs.StringVar(&f.envFile, "env-file", "", "path to .env (default: .env next to kindling.yaml)")
}

func (f *specFlags) envPath() string {
	if f.envFile != "" {
		return f.envFile
	}
	return filepath.Join(filepath.Dir(f.file), ".env")
}

// load reads and validates the spec, printing any problems. It fails if there
// are errors; warnings are printed and ignored.
func (f *specFlags) load() (*spec.Spec, spec.Env, backend.Backend, error) {
	env, err := spec.ReadDotenv(f.envPath())
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := spec.Load(f.file, env)
	if err != nil {
		return nil, env, nil, err
	}
	problems := s.Validate()
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	if spec.HasErrors(problems) {
		return nil, env, nil, errReported
	}
	b, err := newBackend(s)
	return s, env, b, err
}

func newBackend(s *spec.Spec) (backend.Backend, error) {
	switch s.Router.Type {
	case "mlflow":
		return mlflow.New(s), nil
	default:
		return nil, fmt.Errorf("router.type %q is not supported (supported: mlflow)", s.Router.Type)
	}
}

// unmetEnv returns the router container settings that the current env lacks.
func unmetEnv(b backend.Backend, s *spec.Spec, env spec.Env) []backend.EnvRequirement {
	var unmet []backend.EnvRequirement
	for _, req := range b.RequiredEnv(s) {
		have, _ := env.Lookup(req.Key)
		if !containsAll(have, req.Value) {
			unmet = append(unmet, req)
		}
	}
	return unmet
}

// containsAll reports whether every comma-separated item of want is in have.
func containsAll(have, want string) bool {
	items := map[string]bool{}
	for _, v := range strings.Split(have, ",") {
		items[strings.ToLower(strings.TrimSpace(v))] = true
	}
	for _, v := range strings.Split(want, ",") {
		if !items[strings.ToLower(strings.TrimSpace(v))] {
			return false
		}
	}
	return true
}

func warnUnmet(unmet []backend.EnvRequirement, envPath string) {
	for _, req := range unmet {
		fmt.Fprintf(os.Stderr, "warning: %s must be %s (%s); `kindling apply` will set it in %s\n", req.Key, req.Value, req.Reason, envPath)
	}
}

func runValidate(args []string) error {
	var f specFlags
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	f.register(fs)
	explain := fs.Bool("explain", false, "print the effective middleware chain for every endpoint, or only the endpoint named after the flags")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: kindling validate [-f kindling.yaml] [-explain [endpoint]]")
		fs.PrintDefaults()
	}
	parseFlags(fs, args)

	s, env, b, err := f.load()
	if err != nil {
		return err
	}
	if *explain {
		return s.Explain(os.Stdout, fs.Arg(0))
	}
	warnUnmet(unmetEnv(b, s, env), f.envPath())
	summary := fmt.Sprintf("%d connections, %d models, %d endpoints", len(s.Connections), len(s.Models), len(s.Endpoints))
	if s.Middleware != nil {
		summary += fmt.Sprintf(", %d middleware stages", len(s.Middleware.Stages))
	}
	fmt.Printf("%s is valid: %s\n", f.file, summary)
	return nil
}

func runPlan(ctx context.Context, args []string, apply bool) error {
	var f specFlags
	var opts backend.Options
	name := map[bool]string{false: "plan", true: "apply"}[apply]
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	f.register(fs)
	fs.BoolVar(&opts.Prune, "prune", false, "delete kindling-created objects that are no longer in kindling.yaml")
	fs.BoolVar(&opts.RotateSecrets, "rotate-secrets", false, "re-send every API key, even ones that look unchanged")
	var detailedExit bool
	if !apply {
		fs.BoolVar(&detailedExit, "detailed-exitcode", false, "exit 0 if the router matches kindling.yaml, 2 if there are changes, 1 on errors")
	}
	parseFlags(fs, args)

	s, env, b, err := f.load()
	if err != nil {
		return err
	}

	if unmet := unmetEnv(b, s, env); len(unmet) > 0 {
		if !apply {
			warnUnmet(unmet, f.envPath())
		} else {
			for _, req := range unmet {
				if err := spec.SetDotenv(f.envPath(), req.Key, req.Value); err != nil {
					return err
				}
				fmt.Printf("set %s=%s in %s (%s)\n", req.Key, req.Value, f.envPath(), req.Reason)
			}
			fmt.Println("\nRestart the router so it picks these up, then run `kindling apply` again:\n  docker compose up -d --wait")
			return errReported
		}
	}

	plan, err := b.Plan(ctx, s, opts)
	if err != nil {
		return fmt.Errorf("reading router state from %s: %w", s.Router.URL, err)
	}
	printPlan(os.Stdout, s, plan, opts)
	if len(plan.Changes) == 0 {
		return nil
	}
	if !apply {
		if detailedExit {
			return errChanges
		}
		return nil
	}

	fmt.Println()
	err = plan.Apply(ctx, func(c backend.Change) {
		fmt.Printf("%sing %s %s\n", strings.TrimSuffix(string(c.Action), "e"), c.Kind, c.Name)
	})
	if err != nil {
		return err
	}
	fmt.Printf("\nDone. Changes reach every MLflow worker within ~10s (MLFLOW_SECRETS_CACHE_TTL).\n"+
		"OpenAI-compatible clients can use %s/gateway/mlflow/v1 with model set to an endpoint name.\n", s.Router.URL)
	return nil
}

func printPlan(w io.Writer, s *spec.Spec, plan *backend.Plan, opts backend.Options) {
	fmt.Fprintf(w, "%s router at %s\n\n", s.Router.Type, s.Router.URL)
	if len(plan.Changes) == 0 {
		fmt.Fprintln(w, "  no changes; the router matches kindling.yaml")
	}
	for _, c := range plan.Changes {
		fmt.Fprintln(w, " ", c)
	}
	if len(plan.Unmanaged) > 0 {
		fmt.Fprintf(w, "\nNo longer in kindling.yaml (run with -prune to delete):\n  %s\n", strings.Join(plan.Unmanaged, "\n  "))
	}
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory to write into")
	force := fs.Bool("force", false, "overwrite docker-compose.yaml and kindling.yaml (never .env)")
	composeOnly := fs.Bool("compose", false, "only rewrite docker-compose.yaml, to match kindling.yaml's middleware section")
	parseFlags(fs, args)

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	env, err := initialEnv()
	if err != nil {
		return err
	}
	write := func(name string, data []byte, perm os.FileMode, overwrite bool) error {
		path := filepath.Join(*dir, name)
		if _, err := os.Stat(path); err == nil && !overwrite {
			fmt.Printf("kept     %s (already exists)\n", path)
			return nil
		}
		if err := os.WriteFile(path, data, perm); err != nil {
			return err
		}
		fmt.Printf("wrote    %s\n", path)
		return nil
	}

	specPath, envPath := filepath.Join(*dir, "kindling.yaml"), filepath.Join(*dir, ".env")
	if !*composeOnly {
		if err := write("kindling.yaml", mlflowrouter.Starter, 0o644, *force); err != nil {
			return err
		}
		if err := write(".env", []byte(env), 0o600, false); err != nil {
			return err
		}
	}
	// The compose file follows whatever kindling.yaml is there now.
	opts, err := composeOptions(specPath, envPath)
	if err != nil {
		return err
	}
	if err := write("docker-compose.yaml", mlflowrouter.Compose(opts), 0o644, *force || *composeOnly); err != nil {
		return err
	}
	if *composeOnly {
		return nil
	}

	fmt.Print(`
Next:
  1. Edit kindling.yaml (and add API keys to .env if you use hosted models)
  2. docker compose up -d --wait
  3. kindling apply
`)
	return nil
}

// composeOptions picks docker-compose.yaml's optional services for the
// kindling.yaml at path.
func composeOptions(path, envPath string) (mlflowrouter.ComposeOptions, error) {
	env, err := spec.ReadDotenv(envPath)
	if err != nil {
		return mlflowrouter.ComposeOptions{}, err
	}
	s, err := spec.LoadForEdge(path, env)
	if err != nil {
		return mlflowrouter.ComposeOptions{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return mlflowrouter.ComposeOptions{Edge: s.Middleware != nil, PII: s.Middleware.HasStage(spec.StagePII)}, nil
}

// initialEnv fills env.example with fresh secrets and the gateway settings the
// starter kindling.yaml needs.
func initialEnv() (string, error) {
	env := string(mlflowrouter.EnvExample)
	for _, key := range []string{"POSTGRES_PASSWORD", "MLFLOW_CRYPTO_KEK_PASSPHRASE"} {
		env = spec.SetDotenvValue(env, key, randomHex(32))
	}

	starter, err := spec.Parse(mlflowrouter.Starter, spec.Env{})
	if err != nil {
		return "", fmt.Errorf("starter kindling.yaml: %w", err)
	}
	for _, req := range mlflow.New(starter).RequiredEnv(starter) {
		env = spec.SetDotenvValue(env, req.Key, req.Value)
	}
	return env, nil
}

// randomHex needs no error path: since Go 1.24, crypto/rand.Read never fails.
func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
