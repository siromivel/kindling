# kindling middleware — v0 spec

Status: draft for review · Target: v0 implementation · Owner: Levi

## 1. Why

Caching, PII redaction and guardrails for LLM traffic exist today mostly as **libraries you wire into an app** (NeMo Guardrails, Guardrails AI, Presidio, GPTCache) or as features **bundled inside one-stop-shop gateways** (LiteLLM, Portkey). Envoy AI Gateway is the closest thing to "infrastructure, not product" but is heavyweight for a single box.

The gap: a small, standalone middleware proxy where each concern is an **independent, pluggable stage**, configured declaratively, sitting in front of **any OpenAI-compatible router**. Envoy filter-chain thinking applied to LLM traffic.

This fits kindling's existing philosophy: one declarative spec, predictable behavior, secure defaults and no monolith. Each stage is backed by a separate component and kindling only orchestrates them.

## 2. Goals (v0)

1. A proxy that speaks the OpenAI chat completions API and forwards to the configured router (MLflow AI Gateway today).
2. A **stage interface** that supports request-phase and response-phase stages from day one, including streaming, even though v0 only ships request-phase stages.
3. Two stages:
   - **pii**: request-phase PII redaction backed by Microsoft Presidio.
   - **cache**: exact-match response cache.
4. Explicit **fail-open / fail-closed** behavior per stage, set in config.
5. **Observability per stage**: one span per stage, attached to the request's trace.
6. Integration tests in the style of the existing MLflow integration test.

## 3. Non-goals (v0)

- Response-phase stages (output redaction, output guardrails). The interface must support them; no implementations yet.
- Guard models (Llama Guard etc.) and guardrail frameworks.
- Semantic caching.
- Auth, multi-tenancy, rate limiting, budgets.
- Reconciling middleware into the router. Middleware lives in the proxy, not in MLflow.

## 4. Architecture

```
client ──OpenAI API──▶ kindling edge  ──OpenAI API──▶ router (MLflow AI Gateway) ──▶ models
                         │  stage chain:
                         │   request:  pii → cache(lookup)
                         │   response: cache(store) [+ future stages]
                         │
                         ├──▶ Presidio analyzer (container)
                         └┄┄ spans ┄┄▶ MLflow tracing
```

- **One binary.** The proxy (called "the edge" in docs) is a subcommand of the existing CLI: `kindling serve`. That matches the CLI's verb-style commands and the familiar `ollama serve` / `vllm serve` convention. No second artifact.
- **Compose.** `kindling init` adds an `edge` service when a `middleware` section exists, and adds Presidio containers **only when a `pii` stage is configured**. Not every system handles PII, and that's the operator's call. Images pinned by tag and digest in `.env`, same as MLflow and Postgres.
- **All endpoints by default.** Every configured stage applies to every request, so the safe behavior requires no extra config. Per-endpoint overrides (§5) narrow or disable stages where needed. Requests for a `model` that isn't a kindling-managed endpoint (e.g. one made by hand in the MLflow UI) also get the global stages.
- **Binding.** Loopback only by default (`127.0.0.1`), matching the gateway's stance. Same `*_BIND` opt-out pattern.
- **Routing is untouched.** The proxy forwards the `model` field as-is. Endpoint names, splits and fallback remain the router's job.

## 5. Configuration

New top-level `middleware` section in `kindling.yaml`. Stages run in the order listed.

```yaml
middleware:
  listen: 127.0.0.1:${KINDLING_PROXY_PORT:-5060}
  upstream: ${router.url}            # defaults to the router url
  stages:
    - type: pii
      on_error: fail_closed          # required; no default
      policy: standard               # required; default policy for every endpoint
    - type: cache
      on_error: fail_open
      mode: exact
      ttl: 1h
      max_entries: 10000
      # cache_salt: "2026-10"        # change to force-flush the whole cache
      # allow_unredacted_keys: true  # required only if cache is listed before pii

  pii_policies:                      # named, complete, self-contained policies
    standard:
      roles: [user, system]          # message roles to scan
      entities:                      # only listed entities are detected
        US_SSN:        { operator: replace, score_threshold: 0.3 }
        CREDIT_CARD:   { operator: replace, score_threshold: 0.3 }
        EMAIL_ADDRESS: { operator: replace }
        PHONE_NUMBER:  { operator: mask }      # e.g. ***-***-1234
        PERSON:        { operator: keep }      # detect and count, don't redact
    strict:                          # for endpoints that reach third-party APIs
      roles: [user, system, assistant]
      entities:
        US_SSN:        { operator: replace, score_threshold: 0.3 }
        CREDIT_CARD:   { operator: replace, score_threshold: 0.3 }
        EMAIL_ADDRESS: { operator: replace }
        PHONE_NUMBER:  { operator: replace }
        PERSON:        { operator: replace, score_threshold: 0.5 }
        LOCATION:      { operator: replace }

  overrides:                         # per-endpoint; keyed by endpoint name
    chat:
      pii:   { policy: strict }
      cache: { enabled: false }      # A/B testing the 80/20 split; don't pin answers
    local:                           # local model only; data never leaves the box
      pii:   { enabled: false }
```

Policies and overrides:

- **Nothing merges.** A policy is complete and self-contained: reading it tells you exactly what it does. This mirrors how IaC and Kubernetes tooling favors explicit, named objects over layered partial config.
- Every entity entry sets `operator` explicitly. `score_threshold` defaults to 0.5 when omitted; that default is fixed and documented, not configurable.
- Stages are defined once, globally, and apply to all endpoints. An override can do exactly two things per stage: `enabled: false` to skip it for that endpoint, or (for `pii`) `policy: <name>` to select a different named policy. Any other key in an override is a validation error.
- Overrides can't add stages or change stage order. Ordering stays global so the ordering rule below can't be bypassed per endpoint.
- Disabling a `fail_closed` stage via override is allowed, since it's explicit in the file, but `kindling validate` prints a notice listing the endpoints where `pii` is disabled.
- Repetition between policies is accepted in v0 as the price of readability. Policy inheritance (`extends:`) is a planned follow-up (§13), to be added only with clearly specified semantics.

Explain:

- `kindling validate --explain` prints the **effective chain for every endpoint**: which stages run and in what order, the selected policy, each entity's resolved operator and threshold, and whether caching is on. `kindling validate --explain <endpoint>` limits it to one.
- It's the `plan` of middleware: what you see is exactly what the edge will do. Output is deterministic (sorted) so it can be diffed in review or CI.

Rules:

- `on_error` is **required** for every stage. Validation fails if missing. Behavior on stage failure must never be an accident of the code.
- **Stage order is configurable, but risky orders must be explicit.** If a `cache` stage appears before a `pii` stage, validation fails unless that cache stage sets `allow_unredacted_keys: true`. The default and recommended order is `pii` → `cache`. Same philosophy as `on_error`: a risky choice is allowed, never accidental.
- `kindling validate` checks the section: known stage types, required keys, unknown entity names, unknown operators, the ordering rule above, references to undefined policies, unused policies (notice, not error), and override keys that don't match a stage type, an allowed override field or an endpoint in `endpoints`.
- `kindling plan` / `apply` do not touch middleware in v0. Config changes require a proxy restart (SIGHUP reload is a stretch goal).

## 6. Stage interface

Design the interface so response-phase and streaming stages slot in later without changing the request-phase API.

```go
type Phase int
const (
    PhaseRequest Phase = iota
    PhaseResponse
)

type Stage interface {
    Name() string
    Phases() []Phase
    OnError() ErrorPolicy                    // FailOpen | FailClosed

    // Request phase. May mutate the request, or short-circuit with a
    // complete response (e.g. a cache hit).
    OnRequest(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

    // Response phase, non-streaming.
    OnResponse(ctx context.Context, req *ChatRequest, resp *ChatResponse) error

    // Response phase, streaming. Called per SSE chunk. Stages that need
    // context across chunks hold their own window. Returning nil chunks
    // means "buffered, emit later"; Flush is called at end of stream.
    OnChunk(ctx context.Context, req *ChatRequest, chunk *ChatChunk) ([]*ChatChunk, error)
    Flush(ctx context.Context, req *ChatRequest) ([]*ChatChunk, error)
}
```

A no-op base type lets stages implement only what they need.

Chain semantics:

- Request phase runs in config order. The first stage that returns a response short-circuits the rest of the request phase and the upstream call; response-phase stages still run on that response.
- Response phase runs in **reverse** config order (onion model, like Ring/Rack).
- On stage error: `fail_closed` → return an OpenAI-shaped error to the client (HTTP 503, `type: "middleware_error"`, stage name in the message, no internal detail). `fail_open` → log, record on the span, skip the stage, continue.
- Streaming passthrough must flush each chunk immediately when no streaming stage is buffering (`http.Flusher`).

## 7. Stage: pii (v0)

- Backend: the Presidio **analyzer** REST container, pinned, for detection. The edge applies the operators itself; there's no anonymizer container. The anonymizer sets `mask` once per entity type (a fixed `chars_to_mask`, which also masks separators), so it can't keep exactly the last 4 characters of matches whose length varies: with `chars_to_mask: 8`, `(212) 555-0123` comes out as `********5-0123`. Doing it in-process also removes a container, a network hop per text, and a dependency that can fail closed.
- **Offsets.** Presidio's offsets index Python strings, so they count code points. The stage normalizes each text to valid UTF-8 once (each run of invalid bytes becomes U+FFFD), sends that exact string to the analyzer, and slices that same string as Go runes. The two sides must never apply different normalizations: Go's JSON decoder, for one, replaces each invalid *byte*, not each run, and mixing the two shifts every later offset.
- **Overlaps.** Overlapping or touching `replace`/`mask` matches merge into one redacted span covering all of them. The span gets the most protective operator among them (`replace` over `mask`), so a mask can never leave part of something another detection replaces visible. A `keep` match never protects text that overlaps a redaction.
- Scans `content` of messages whose role is in `roles`. Handles string content and text parts of multi-part content; skips non-text parts.
- **Named policies.** The stage applies the named policy selected for the request's endpoint (global `policy`, or an override). Each policy lists entity types with their own `operator` and `score_threshold`; unlisted entities are not detected. Sensitivity differs by entity and by deployment, so this is a policy table rather than one global switch.
- Operators (v0):
  - `replace`: substitute `<ENTITY_TYPE>`.
  - `mask`: partial masking. It keeps the last 4 characters and separators (e.g. `***-***-1234`) and masks letters and digits. Matches shorter than 8 characters are masked entirely, so masking never reveals most of a short value. `mask` is meant for numbers (phones, cards, SSNs). On names and addresses the last 4 characters give away too much ("John Smith" → "**** *mith"), so `kindling validate` prints a notice for `mask` on `PERSON`, `LOCATION`, `NRP`, `EMAIL_ADDRESS` and `URL`: use `replace` for those.
  - `keep`: detect and count on the span, but forward unchanged. Useful for auditing an entity before deciding to redact it.
- Lower `score_threshold` means more aggressive detection. High-sensitivity entities (SSN, card numbers) should use lower thresholds; noisy ones (PERSON, LOCATION) higher. Scores depend on context: Presidio scores a bare phone number 0.4, and 0.75 with a word like "phone" nearby, so the default threshold of 0.5 misses most phone numbers.
- Default `fail_closed`: if Presidio is unreachable or errors, the request is blocked, never sent upstream unredacted.
- Records on the span: policy name, entity types, counts and the operator applied. **Never** record the original text or matched values.
- Tool-call arguments and tool results: out of scope for v0, documented as such.

## 8. Stage: cache (v0)

- Mode `exact` only.
- Key = SHA-256 over a canonical JSON encoding of: `model`, normalized `messages` (role + content, whitespace-normalized), sampling params (`temperature`, `top_p`, `max_tokens`, `stop`, `seed`, `tools`, `tool_choice`, `response_format`), the **route fingerprint** and `cache_salt`.
- **Route fingerprint.** A hash of the sorted set of `(connection type, model id)` across the endpoint's `route` and `fallback` in `kindling.yaml`. Changing which models stand behind an endpoint invalidates its cached answers automatically. Deliberately **excluded**: weights, fallback order, credentials and anything else that doesn't change what the answers could be, so tuning a split or rotating a key doesn't flush a valid cache.
- **Limits of the fingerprint**, to document in the README: it can't see endpoints edited by hand in the MLflow UI, or a server swapping weights under the same model name. TTL bounds that staleness, and `cache_salt` forces a flush.
- **Caching vs traffic splits.** A cached answer pins whichever model produced it first, so a split endpoint effectively stops splitting for repeated prompts. README should say so and recommend disabling the cache on endpoints used for model comparison.
- **Ordering matters.** With the default `pii` → `cache` order, the key and the stored entry are computed on the redacted request, so raw PII never enters the cache. A side benefit: prompts that differ only by a redacted value (two different email addresses, say) normalize to the same key and share a hit.
- **Cache before pii** (requires `allow_unredacted_keys: true`) skips the Presidio round-trip on hits. The costs: keys are hashes of raw text, and hashes of low-entropy values like SSNs and phone numbers can be brute-forced; the hit rate drops, because any difference in PII produces a different key; and if the response echoes PII back, the stored response contains it. Document these in the README next to the flag.
- Entities with `operator: keep` are not redacted, so they reach the cache key regardless of order. That's by design and should be noted in the docs.
- Don't cache: requests with `n > 1`, error responses, or when `cache-control: no-cache` is sent by the client.
- Storage: in-process LRU with TTL for v0, behind a `Store` interface so Redis or Valkey can be added without touching the stage.
- Streaming requests: on a hit, replay the cached response as a synthetic SSE stream; on a miss, tee chunks and store the assembled response at end of stream.
- Response header `x-kindling-cache: hit | miss | bypass`.
- Default `fail_open`: a cache error never blocks traffic.

## 9. Observability

- One span per stage per request, attached to the request's trace, with attributes: stage name, phase, outcome (`pass`, `hit`, `miss`, `redacted`, `blocked`, `error_failed_open`) and duration.
- Export via OpenTelemetry to MLflow tracing. **Verify** the OTLP ingestion path on the pinned MLflow version first. If it's not usable, fall back to structured JSON logs in v0 and track the trace integration as follow-up.
- Goal for the demo: one request in the MLflow UI shows "pii: redacted 2 → cache: miss → upstream → cache: stored."

## 10. Testing

Unit:
- Chain ordering, short-circuit and reverse response order.
- Fail-open vs fail-closed for each stage, with injected errors.
- Cache key canonicalization: equivalent requests hit; any param change misses.
- Validation: `cache` before `pii` fails without `allow_unredacted_keys`, passes with it; unknown entity or operator fails.
- PII policy: per-entity thresholds and operators apply; `keep` counts without modifying; `mask` keeps the last 4 characters; `mask` on name-like entities prints a notice.
- PII offsets: characters outside the BMP (emoji), ZWJ sequences, combining marks and invalid UTF-8 don't shift redactions (also checked against the real analyzer in integration).
- PII leak property, fuzzed: for random text and random overlapping matches, no character inside a `replace` or `mask` match reaches the output, except the last 4 characters (and separators) of an all-`mask` group.
- Policies and overrides: an endpoint gets exactly the selected policy, with no merging; `enabled: false` skips the stage; any override field other than `enabled` or `policy` fails validation; unknown endpoint, stage or policy name fails validation; overrides can't reorder or add stages.
- Explain: `--explain` output for a fixture config matches a golden file, including disabled stages and per-endpoint policy selection; output is stable across runs.
- Route fingerprint: changing a model in the route changes the key; changing weights, fallback order or credentials does not; `cache_salt` changes the key.
- Streaming passthrough flushes per chunk.

Integration (extends the existing Docker-based test, same `-tags integration` flow):
1. Stub upstream records the exact messages it received.
2. **pii:** request containing an email and phone number → stub receives `<EMAIL_ADDRESS>` and a masked phone number per the policy; span shows types, counts and operators, never values.
2b. **pii + cache sharing:** two requests identical except for the email address → stub called once; second is a cache hit.
3. **pii fail-closed:** stop the Presidio container → request returns 503, stub receives nothing.
4. **cache:** same request twice → stub called once; second response has `x-kindling-cache: hit`.
5. **cache fail-open:** injected store error → request still succeeds.
6. **streaming:** streamed request passes through chunk-by-chunk; cached replay streams correctly.
7. **overrides:** with `pii` disabled on `local`, a request to `local` reaches the stub unredacted; the same request to `chat` (policy `strict`) has `PERSON` replaced, while a third endpoint on the default `standard` policy forwards `PERSON` unchanged (`keep`); with `cache` disabled on `chat`, repeated requests to `chat` always reach the stub.
8. **no Presidio when no pii:** `kindling init` with only a `cache` stage emits no Presidio services, and the stack starts.
9. **route invalidation:** cache a response, change the model behind the endpoint in `kindling.yaml`, restart the edge → next request is a miss.

## 11. Layout

```
internal/middleware/            chain, Stage interface, error policy, spans
internal/middleware/pii/        Presidio analyzer client, operators, stage
internal/middleware/cache/      key canonicalization, Store interface, LRU store, stage
internal/proxy/                 HTTP server, OpenAI request/response types, SSE handling
cmd/kindling/                   `serve` subcommand
routers/mlflow/                 compose additions for proxy + Presidio
```

## 12. Acceptance criteria

- `kindling init` with middleware enabled produces a stack that starts with `docker compose up -d --wait`.
- `kindling validate` rejects a stage without `on_error`, and rejects `cache` before `pii` unless `allow_unredacted_keys: true`.
- `kindling validate --explain` prints the effective per-endpoint chain.
- All integration scenarios in §10 pass.
- README gains a "Middleware" section: what v0 does, the fail-open/fail-closed table and the explicit non-goals.

## 13. Future (not v0)

- Response-phase PII redaction and output guardrails using the streaming interface (sliding window over chunks).
- Restoring redacted values in responses. This needs no encryption: redact to indexed placeholders (`<EMAIL_ADDRESS_1>`), keep a placeholder → original table in memory for the life of the request, and substitute back in the response phase. There's no key management, the originals never leave the process, and indexed placeholders help the model keep two values apart. The index is per request, so requests that differ only in a redacted value still share a cache key.
- Guard-model stage (e.g. Llama Guard on local GPU) with a configurable policy.
- Semantic cache behind a flag with a tunable similarity threshold.
- Redis or Valkey cache store.
- `plan` / `apply` awareness of middleware, and SIGHUP reload.
- **Policy inheritance** (`extends: <policy>`), added once v0 is solid, with explicitly specified semantics: entity entries replace by key, `roles` replaces wholesale, single-level only at first, and `--explain` always shows the fully resolved result.
- Named policies for other stages (e.g. cache profiles) if per-endpoint cache needs grow beyond `enabled`.

## 14. Decisions

1. **Endpoint scope:** all stages apply to all endpoints by default; narrow with per-endpoint `overrides`. Simple to start, safe by default. (§4, §5)
2. **Presidio in compose:** emitted only when a `pii` stage is configured. Whether a system needs PII handling is the operator's call. (§4)
3. **Route-change invalidation:** yes, via a route fingerprint of the models behind each endpoint, excluding weights, fallback order and credentials, plus a manual `cache_salt`. (§8)
4. **Subcommand name:** `kindling serve`; the component is called "the edge" in docs. (§4)
5. **Per-endpoint PII policy:** yes. Endpoints select from **named, self-contained policies**; overrides can only pick a policy or disable a stage. No merge semantics in v0, and `--explain` shows the effective chain. Inheritance is deferred until the MVP is working. Requirements depend on what each system handles. (§5)
6. **Operators in-process, not the Presidio anonymizer:** the anonymizer can't express "keep the last 4" for variable-length matches, and restoring values later (§13) works better with a per-request placeholder table than with the anonymizer's encrypt/decrypt. The analyzer still does all the detection. (§7)
7. **The edge always rebuilds the request; it never forwards the client's original bytes,** not even when no stage changed anything. Whatever the router receives is exactly what the stages saw: model and messages re-encoded, every other field as its raw JSON value. Forwarding the original bytes would save little bandwidth or latency and reopens the gap between what the edge checked and what the router reads. Don't add it. **Duplicate keys** in the request body, a message, or a content part are rejected with a 400 before any stage runs, instead of being settled silently by the decoder. Otherwise one copy could pass through pii while the router reads the other. Keys are compared after unescaping, so `"messages"` and `"messag\u0065s"` count as the same key. (§6)
