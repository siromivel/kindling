# AGENTS.md

This file provides guidance to LLMs when working with code in this repository.

## What this is

kindling is a Go CLI (`cmd/kindling`) plus a pinned Docker Compose stack for self-hosting an LLM router. Users describe connections, models and endpoints in `kindling.yaml`, and `kindling apply` reconciles the router to match, Terraform-style (`plan` / `apply` / `-prune`). The only router so far is the MLflow 3.17 AI Gateway, driven through its DB-backed REST API (`/api/3.0/mlflow/gateway/...`). The deprecated YAML `mlflow gateway start` path is deliberately unsupported.

## Commands

```sh
go test ./...                                                 # unit tests (no Docker)
go test ./internal/mlflow -run TestPrune -v                   # a single test
go test -tags integration -timeout 15m -v ./cmd/kindling      # real MLflow + Postgres stack in Docker
KINDLING_IT_KEEP=1 go test -tags integration ...              # leave the stack running for debugging
```

CI (`.github/workflows/ci.yml`) runs `gofmt -l .` (must print nothing), `go vet -tags integration ./...`, unit tests, then the integration test. Pass `-tags integration` to vet, or the integration test file won't be checked.

Run the integration test after bumping the pinned image tags/digests in `routers/mlflow/env.example` or the `Dockerfile`. When you bump MLflow, also re-check the `--allowed-hosts` list in the compose template against `mlflow.server.security_utils.get_default_allowed_hosts()`.

## Architecture

The data flows: `kindling.yaml` + `.env` → `spec.Spec` → `backend.Backend.Plan` → ordered `[]backend.Change` → `Plan.Apply`.

- **`internal/spec`** does not depend on any router. It handles parsing (strict: `KnownFields(true)`), `${VAR}` / `${VAR:-default}` interpolation, and validation. `Env` reads `.env`, and process environment variables override it, as in docker compose. `ConnectionTypes` in `connections.go` is the registry of connection types and their api_key/base_url rules. Keep router-specific concepts out of this package so other routers (e.g. LiteLLM) can be added later.
- **`internal/backend`** holds the interface each router implements: `RequiredEnv` (container settings the spec needs) and `Plan`. A `Change` carries its own `Run` closure, and changes must run in plan order.
- **`internal/mlflow`** maps kindling concepts onto MLflow: connection → *secret*, model → *model definition*, endpoint → *endpoint* with PRIMARY/FALLBACK model mappings. Things to know about the planner (`backend.go`):
  - It runs in dependency order: secrets → models → endpoints, then prune in reverse (endpoints → models → secrets). `Run` closures write newly created IDs back into `secretIDs`/`modelIDs` so later changes can resolve them. That is why request builders are closures (`req := func() ...`) evaluated at run time.
  - **Ownership:** everything kindling creates gets `created_by: kindling`. Objects with a matching name that kindling didn't create are an error, never modified. Prune only touches kindling-owned objects.
  - **Credential drift:** secrets can't be read back. `maskValue` mirrors MLflow's `_mask_string_value`, so plaintext from the spec is compared against MLflow's masked values. `-rotate-secrets` skips this comparison and re-sends every key.
  - MLflow update semantics: an empty map or unset field means "unchanged", so to clear `api_base` you send it explicitly as blank. Provider changes can't be done in place.
  - `needsPrivateNetwork` predicts MLflow's SSRF guard. `RequiredEnv` turns that into `MLFLOW_GATEWAY_API_BASE_ALLOW_PRIVATE_IPS` / `..._ALLOWED_SCHEMES`. When those settings are missing, `apply` writes them into `.env` and stops (returns `errReported`), and the user restarts the stack.
  - `openai-compatible` connections are stored as provider `openai`. Keyless connections get the placeholder key `kindling-no-api-key`.
- **`internal/mlflow/fake_test.go`** is an in-memory fake of the gateway REST API that follows MLflow's update semantics. Unit tests for the reconciler run against it. If you change client behavior, keep the fake faithful to real MLflow, and confirm with the integration test.
- **Middleware (the edge, `kindling serve`)** is a proxy in front of the router, configured by `kindling.yaml`'s `middleware` section. `plan`/`apply` never touch it.
  - `internal/spec/middleware.go` holds the schema, validation, `Middleware.Chain(endpoint)` (the effective stages after overrides) and `RouteFingerprint`. `explain.go` is `validate -explain`, golden-tested in `testdata/explain.golden` (`go test ./internal/spec -run Golden -update`). `Stage`/`Override` record their YAML keys, so per-type and unknown keys are validation errors rather than parse errors.
  - `internal/middleware` owns the OpenAI types (`ChatRequest` keeps unknown fields raw in `Params`), the `Stage` interface plus a no-op `Base`, and `Chain`/`Run`. A run executes the request phase in order and the response phase in reverse, only over stages before one that short-circuited. Fail-open stages are skipped for the rest of the request. A run records one span per stage per phase. Spans go to MLflow as OTLP/JSON (`/v1/traces`, experiment `kindling-edge`).
  - `pii` uses only Presidio's analyzer, and applies operators itself over **code-point** offsets. `ValidUTF8` normalizes each text once, and the analyzer and `Redact` must get that same string. Merged overlaps take the most protective operator. `FuzzRedactNoLeak` checks the leak property (`go test ./internal/middleware/pii -fuzz FuzzRedactNoLeak`), so run it after touching `redact.go`. `cache` keys include every parameter except an ignore list, plus `Authorization`, so it errs toward misses. `proxy` serves only `POST /v1/chat/completions` and flushes every SSE chunk.
  - `cmd/kindling/serve.go` `newEdge` builds one pii stage per policy and one shared cache stage, then a chain per endpoint. The edge parses with `spec.ParseForEdge`, which never expands connection credentials, and its container gets no `.env`.
  - `init` renders `routers/mlflow/docker-compose.yaml.tmpl`. The edge appears only with a `middleware` section, and Presidio only with a pii stage. `init -compose` regenerates just the compose file. The edge image comes from the repo `Dockerfile`, and nothing is published yet.
- **`routers/mlflow`** embeds (`go:embed`) the compose file, `env.example` and the starter `kindling.yaml` that `kindling init` writes. `init` fills in random secrets and the `RequiredEnv` of the starter spec. It never overwrites `.env`.
- **`cmd/kindling/main.go`** uses the stdlib `flag` package and has a hand-rolled subcommand switch. Exit codes matter: 2 = `plan -detailed-exitcode` found changes (`errChanges`), 1 = any error, including bad flags (`parseFlags` deliberately avoids flag's default exit 2). `errReported` means the message was already printed.
- **`cmd/kindling/integration_test.go`** (`//go:build integration`) calls `runInit` / `runPlan` directly against a real compose stack on a free port. A stub OpenAI-compatible server inside the test stands in for model providers. `middleware_integration_test.go` builds the edge image from the checkout (`kindling-it:latest`) and runs a second stack with Presidio and the edge. Scenarios that need injected faults or a store shared across restarts run `newEdge` in-process against the same MLflow.

## Project direction

- Build the happy path first. Don't add legacy or compat paths, or edge-case support, until users ask for them.
- Safety defaults are part of the product: images pinned by tag and digest, gateway bound to `127.0.0.1`, credentials encrypted with `MLFLOW_CRYPTO_KEK_PASSPHRASE`, private networking opt-in. Don't weaken them for convenience.
- `docs/specs/` holds design specs. The middleware v0 spec is implemented. Where the code deviates from it (no anonymizer container, cache key params), the README's Middleware section describes the current behavior.
