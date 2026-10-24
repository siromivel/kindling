# kindling

Scaffolding for running your own LLM router: a pinned, secure-by-default Docker
Compose stack, plus a CLI that configures the router from one file.

You describe connections, models and endpoints in `kindling.yaml`, and
`kindling apply` makes the router match it. Running it again with no changes
does nothing.

The first supported router is the [MLflow AI Gateway](https://mlflow.org/docs/latest/genai/governance/ai-gateway/)
(MLflow 3.17), backed by Postgres.

## Quick start

```sh
go install github.com/siromivel/kindling/cmd/kindling@latest

mkdir my-router && cd my-router
kindling init                  # docker-compose.yaml, .env (generated secrets), kindling.yaml
docker compose up -d --wait    # MLflow UI at http://localhost:5050
kindling apply
```

Then point any OpenAI-compatible client at the gateway, using an endpoint name as the model:

```sh
curl http://localhost:5050/gateway/mlflow/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model": "chat", "messages": [{"role": "user", "content": "hi"}]}'
```

## kindling.yaml

```yaml
router:
  type: mlflow
  url: http://localhost:${MLFLOW_PORT:-5050}

connections:                       # credentials for each upstream API
  vllm:
    type: openai-compatible        # vLLM, llama.cpp, LM Studio, SGLang, ...
    base_url: http://host.docker.internal:8000/v1
  ollama:
    type: ollama                   # defaults to Ollama on the Docker host
  anthropic:
    type: anthropic
    api_key: ${ANTHROPIC_API_KEY}  # from the environment or .env

models:
  qwen:   { connection: vllm, model: Qwen/Qwen3.8-27B }
  claude: { connection: anthropic, model: claude-sonnet-5-5 }

endpoints:                         # what clients call
  chat:
    route:                         # split traffic by weight...
      - { model: claude, weight: 80 }
      - { model: qwen, weight: 20 }
    fallback: [qwen]               # ...and try these in order if a model fails
  local:
    route: [qwen]
```

Connection types: `openai-compatible`, `ollama`, `openai`, `anthropic`, `gemini`,
`mistral`, `groq`, `deepseek`, `xai`, `openrouter`. Leave out weights to split
traffic evenly. `usage_tracking` (MLflow traces for each request) is on by
default; set `usage_tracking: false` on an endpoint to turn it off.

## Commands

| Command | What it does |
|---|---|
| `kindling init` | Writes `docker-compose.yaml`, `kindling.yaml`, and `.env` with a fresh Postgres password and credential-encryption passphrase. Never overwrites `.env`. |
| `kindling validate` | Checks `kindling.yaml`: references, weights, required keys, unset `${VARS}`. |
| `kindling plan` | Shows what `apply` would create, update or delete. `-detailed-exitcode` exits 0 if the router matches, 2 if there are changes, 1 on errors: a drift check for CI. Add `-prune` to also count objects removed from the file. |
| `kindling apply` | Makes the router match `kindling.yaml`. `-prune` deletes things you removed from the file; `-rotate-secrets` re-sends every API key. |

## Safety defaults

- **Pinned images.** MLflow and Postgres are pinned by tag *and* digest in `.env`.
- **Loopback only.** The gateway holds your provider keys and has no auth by
  default, so it listens on `127.0.0.1`. Change `MLFLOW_BIND` only once you've put
  auth in front of it.
- **Encrypted credentials.** API keys are encrypted at rest with
  `MLFLOW_CRYPTO_KEK_PASSPHRASE`. Back it up: if it's lost, the stored keys are unreadable.
- **Private networks are opt-in.** MLflow refuses to call private or plain-`http`
  addresses unless told to. When `kindling.yaml` has a local server, `kindling apply`
  turns on the two settings in `.env` that allow it and tells you to restart.
- **Hands off the rest.** kindling marks what it creates and never modifies or
  prunes connections, models or endpoints you set up by hand in the MLflow UI.

## MLflow notes

- **Changes take up to ~10s to reach every worker.** Each MLflow server worker
  caches endpoint settings. The compose file sets the cache TTL to MLflow's
  minimum (`MLFLOW_SECRETS_CACHE_TTL=10`; MLflow's default is 60s).
- **On Linux, local model servers must listen on `0.0.0.0`.** There,
  `host.docker.internal` is the Docker bridge address, not loopback, so a server
  bound to `127.0.0.1` (Ollama's default) can't be reached from the gateway. Start
  it with `OLLAMA_HOST=0.0.0.0 ollama serve`, `vllm serve --host 0.0.0.0`, etc.
  Firewall the port if this machine is on an untrusted network.
- **"Failed to send trace" warnings are harmless.** With usage tracking on, MLflow
  logs this for gateway requests, but the traces and their spans are still stored
  in Postgres and show up in the UI. The integration test checks this.

## Development

```sh
go test ./...                                                 # unit tests
go test -tags integration -timeout 15m -v ./cmd/kindling      # real MLflow stack in Docker
```

The integration test runs `kindling init` into a temp directory, starts the
compose stack on a free port, and drives it with the CLI. A stub
OpenAI-compatible server in the test stands in for model providers. It checks
splits, fallback, credentials, updates, prune and traces. It takes about 35s
once images are pulled, and cleans up after itself; set `KINDLING_IT_KEEP=1` to
leave the stack running for debugging. Run it after bumping the pinned image
digests in `routers/mlflow/env.example`.

## Layout

```
cmd/kindling/       CLI
internal/spec/      kindling.yaml: parsing, ${VAR} expansion, validation (router-agnostic)
internal/backend/   the interface each router implements
internal/mlflow/    MLflow gateway REST client and reconciler
routers/mlflow/     compose file, env template and starter kindling.yaml for `init`
examples/           more kindling.yaml examples
```
