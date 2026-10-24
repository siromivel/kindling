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
| `kindling validate` | Checks `kindling.yaml`: references, weights, required keys, unset `${VARS}`, the middleware section. `-explain [endpoint]` prints the effective middleware chain per endpoint. |
| `kindling plan` | Shows what `apply` would create, update or delete. `-detailed-exitcode` exits 0 if the router matches, 2 if there are changes, 1 on errors: a drift check for CI. Add `-prune` to also count objects removed from the file. |
| `kindling apply` | Makes the router match `kindling.yaml`. `-prune` deletes things you removed from the file; `-rotate-secrets` re-sends every API key. |
| `kindling serve` | Runs the edge, the middleware proxy (see [Middleware](#middleware)). `kindling init` adds it to the compose file, so you rarely run this yourself. |

`kindling init -compose` rewrites only `docker-compose.yaml` to match `kindling.yaml`,
e.g. after adding a middleware section.

## Middleware

The edge (`kindling serve`) is an OpenAI-compatible proxy in front of the router. It
runs every chat request through a chain of independent stages you declare in
`kindling.yaml`. v0 has two stages:

- **pii** redacts PII in request messages before they reach the router, using
  [Presidio](https://microsoft.github.io/presidio/)'s analyzer.
- **cache** answers repeated requests from an exact-match cache.

```yaml
middleware:
  listen: 127.0.0.1:${KINDLING_PROXY_PORT:-5060}   # when you run `kindling serve` yourself
  stages:                        # requests run top to bottom, responses bottom to top
    - type: pii
      on_error: fail_closed      # required on every stage
      policy: standard           # the default policy for every endpoint
    - type: cache
      on_error: fail_open
      ttl: 1h
      max_entries: 10000
      # cache_salt: "2026-10"    # change to empty the cache

  pii_policies:                  # named and complete: policies never merge
    standard:
      roles: [user, system]      # message roles to scan
      entities:                  # only these are detected
        US_SSN:        { operator: replace, score_threshold: 0.3 }
        EMAIL_ADDRESS: { operator: replace }          # score_threshold defaults to 0.5
        PHONE_NUMBER:  { operator: mask, score_threshold: 0.4 }
        PERSON:        { operator: keep }             # count it, don't redact it
    strict:
      roles: [user, system, assistant]
      entities:
        EMAIL_ADDRESS: { operator: replace }
        PHONE_NUMBER:  { operator: replace, score_threshold: 0.4 }
        PERSON:        { operator: replace }

  overrides:                     # per endpoint: only `enabled: false`, or a pii `policy`
    chat:
      pii:   { policy: strict }
      cache: { enabled: false }  # an A/B split: don't pin answers
    local:
      pii:   { enabled: false }  # local model: data never leaves the box
```

Every stage applies to every endpoint, and to models that aren't endpoints in
`kindling.yaml`, unless an override narrows it. Overrides can't add stages or reorder
them. `kindling validate -explain` prints exactly what the edge will do for each
endpoint: stages in order, the policy, each entity's operator and threshold, and
whether caching is on. It's sorted, so you can diff it in review.

To run it:

```sh
docker build -t kindling:local https://github.com/siromivel/kindling.git   # no published image yet
kindling init -compose          # adds the edge (and Presidio, with a pii stage)
docker compose up -d --wait
```

Then point clients at `http://localhost:5060/v1` instead of the gateway. The edge
reads `kindling.yaml` at startup, so after editing the middleware section, run
`docker compose restart edge`. `kindling plan` and `apply` don't touch middleware.
The edge only expands variables in the fields it uses (models, middleware), never
connection credentials, and the container has no `.env`, so give any other `${VAR}` it
needs a default.

### Failure handling

`on_error` is required, so what happens when a stage breaks is always a decision:

| | `fail_closed` | `fail_open` |
|---|---|---|
| A stage fails on a request | The client gets a 503 (`type: middleware_error`, naming the stage) and nothing reaches the router | The error is logged and recorded on the trace, and the stage is skipped for the rest of that request |
| pii, Presidio down | Requests are blocked (**recommended**) | Requests go to the router **unredacted** |
| cache, store failing | Requests are blocked | Requests are answered by the router, uncached (**recommended**) |
| A stage fails mid-stream | The stream ends with an error event and no `[DONE]` | The rest of the stream passes through unchanged |

### pii

- Operators: `replace` writes `<ENTITY_TYPE>`. `mask` keeps the last 4 characters
  and separators (`***-***-0123`); values shorter than 8 characters are masked
  entirely. `keep` forwards the value and only counts it on the trace.
- Use `mask` for numbers (phones, cards, SSNs), not names or addresses: "John Smith"
  would become "**** *mith". `kindling validate` prints a notice when you do.
- Overlapping detections are redacted as one span, with the most protective operator
  among them (`replace` over `mask`), so nothing detected leaks around the edges.
- Lower `score_threshold` means more aggressive detection. Presidio's scores depend
  on context: a bare phone number scores 0.4, rising to 0.75 with a word like "phone"
  nearby, so at the default threshold of 0.5 most phone numbers are missed.
- Scans string content and the text parts of multi-part content. Images and other
  parts aren't scanned. Neither are tool call arguments or tool results (role `tool`)
  in v0.
- Traces record the policy, entity types, counts and operators. They never record the
  matched text.
- Only Presidio's analyzer runs. kindling applies the operators itself, because
  Presidio's anonymizer can't keep the last 4 characters of values whose length
  varies. Each text is normalized to valid UTF-8 once, and that exact string is both
  analyzed and redacted, so Presidio's code-point offsets line up. The analyzer has
  no published port; only the edge reaches it.

### cache

- The key is a SHA-256 over the model, the messages (whitespace-normalized), every
  request parameter that can change the answer (`temperature`, `top_p`, `max_tokens`,
  `stop`, `seed`, `tools`, `tool_choice`, `response_format`, and any others; `stream`,
  `stream_options`, `user`, `metadata` and `store` are ignored), the client's
  `Authorization` header, the endpoint's **route fingerprint** and `cache_salt`.
- The route fingerprint covers the connection type and model id of every route and
  fallback model, so changing which models stand behind an endpoint invalidates its
  answers. Weights, fallback order, base URLs and credentials are left out, so tuning a
  split or rotating a key keeps a valid cache. The fingerprint can't see endpoints
  edited by hand in the MLflow UI, or a server swapping weights under the same model
  name. `ttl` bounds that staleness, and changing `cache_salt` empties the cache.
- **Caching defeats traffic splits.** The first model to answer a prompt answers it
  from then on, so disable the cache on endpoints you use to compare models.
- **Keep the cache after pii** (the default order). Keys and stored answers are then
  computed on redacted requests, so raw PII never enters the cache, and prompts that
  differ only in a redacted value share an entry. Putting the cache first requires
  `allow_unredacted_keys: true`. That saves the Presidio call on hits, but keys become
  hashes of raw text (low-entropy values like SSNs and phone numbers can be
  brute-forced), the hit rate drops, and answers that echo PII are stored with it.
  Entities with `operator: keep` reach the key in either order.
- Not cached: requests with `n > 1` or `cache-control: no-cache`/`no-store`, error
  responses and incomplete answers. Streamed and non-streamed requests share entries.
  A hit on a streamed request is replayed as a stream.
- Responses carry `x-kindling-cache: hit`, `miss` or `bypass`. The store is in-process
  (lost on restart) and sits behind a `Store` interface, so a shared store like Redis
  can be added.

### Traces

Each request is a trace in the MLflow experiment `kindling-edge`. It has one span per
stage per phase, with the stage, phase, outcome (`pass`, `hit`, `miss`, `bypass`,
`stored`, `redacted`, `blocked`, `error_failed_open`) and duration. The
`x-kindling-trace-id` response header holds the trace ID.

### Not in v0

Response-phase stages (output redaction, guardrails), guard models, semantic caching,
auth, rate limits and budgets, and reloading config without a restart. The stage
interface already supports response-phase and streaming stages.

## Safety defaults

- **Pinned images.** MLflow, Postgres and Presidio are pinned by tag *and* digest in `.env`.
- **Loopback only.** The gateway holds your provider keys and has no auth by
  default, so it listens on `127.0.0.1`. Change `MLFLOW_BIND` only once you've put
  auth in front of it. The edge listens on `127.0.0.1` too (`KINDLING_PROXY_BIND`).
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
- **The edge needs an extra allowed host.** MLflow rejects Host headers it doesn't
  know (DNS rebinding protection). With the edge, the compose file passes
  `--allowed-hosts` with MLflow's defaults plus `mlflow:5000`. Update that list
  if you bump MLflow and its defaults change.
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
splits, fallback, credentials, updates, prune and traces. A second stack adds
Presidio and the edge, built from this checkout, and checks every middleware stage
end to end. Together they take about 90s once images are pulled, and clean up after
themselves; set `KINDLING_IT_KEEP=1` to leave the stacks running for debugging. Run
it after bumping the pinned image digests in `routers/mlflow/env.example` or the
`Dockerfile`.

## Layout

```
cmd/kindling/                 CLI, including `serve`
internal/spec/                kindling.yaml: parsing, ${VAR} expansion, validation, -explain (router-agnostic)
internal/backend/             the interface each router implements
internal/mlflow/              MLflow gateway REST client, reconciler and trace export
internal/middleware/          chain, Stage interface, error policy, spans, OTLP export
internal/middleware/pii/      Presidio client and pii stage
internal/middleware/cache/    cache keys, Store interface, LRU store, cache stage
internal/proxy/               the edge's HTTP server and SSE handling
routers/mlflow/               compose template, env template and starter kindling.yaml for `init`
Dockerfile                    the edge image
examples/           more kindling.yaml examples
```
