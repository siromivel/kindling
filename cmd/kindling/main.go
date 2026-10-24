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

Run "kindling <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
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
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "kindling: unknown command %q\n\n%s", cmd, usage)
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
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	f.register(fs)
	fs.Parse(args)

	s, env, b, err := f.load()
	if err != nil {
		return err
	}
	warnUnmet(unmetEnv(b, s, env), f.envPath())
	fmt.Printf("%s is valid: %d connections, %d models, %d endpoints\n", f.file, len(s.Connections), len(s.Models), len(s.Endpoints))
	return nil
}

func runPlan(ctx context.Context, args []string, apply bool) error {
	var f specFlags
	var opts backend.Options
	name := map[bool]string{false: "plan", true: "apply"}[apply]
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	f.register(fs)
	fs.BoolVar(&opts.Prune, "prune", false, "delete kindling-created objects that are no longer in kindling.yaml")
	fs.BoolVar(&opts.RotateSecrets, "rotate-secrets", false, "re-send every API key, even ones that look unchanged")
	fs.Parse(args)

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
	if !apply || len(plan.Changes) == 0 {
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
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory to write into")
	force := fs.Bool("force", false, "overwrite docker-compose.yaml and kindling.yaml (never .env)")
	fs.Parse(args)

	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	env, err := initialEnv()
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name      string
		data      []byte
		perm      os.FileMode
		overwrite bool
	}{
		{"docker-compose.yaml", mlflowrouter.Compose, 0o644, *force},
		{"kindling.yaml", mlflowrouter.Starter, 0o644, *force},
		{".env", []byte(env), 0o600, false},
	} {
		path := filepath.Join(*dir, f.name)
		if _, err := os.Stat(path); err == nil && !f.overwrite {
			fmt.Printf("kept     %s (already exists)\n", path)
			continue
		}
		if err := os.WriteFile(path, f.data, f.perm); err != nil {
			return err
		}
		fmt.Printf("wrote    %s\n", path)
	}

	fmt.Print(`
Next:
  1. Edit kindling.yaml (and add API keys to .env if you use hosted models)
  2. docker compose up -d --wait
  3. kindling apply
`)
	return nil
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
