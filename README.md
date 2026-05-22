# LLM0 Gateway

[![Go](https://img.shields.io/badge/Go-1.24-blue?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-Compose-blue?logo=docker)](docker-compose.yml)
[![OpenAI Compatible](https://img.shields.io/badge/API-OpenAI_Compatible-412991)](https://platform.openai.com/docs/api-reference)

A production-grade, self-hosted LLM gateway written in Go. One **OpenAI-compatible** API endpoint for **OpenAI**, **Anthropic**, **Google Gemini**, and **local Ollama models** — with configurable cloud/local failover, two-tier caching, streaming, per-key rate limiting, per-customer spend caps, and cost tracking out of the box.

```bash
curl -i http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llm0_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"What is the capital of France?"}]}'
```

Response:

```http
HTTP/1.1 200 OK
Content-Type: application/json; charset=utf-8
X-Provider: openai
X-Cache-Hit: miss
X-Cost-Usd: 0.000007
X-Tokens-Prompt: 16
X-Tokens-Completion: 7
X-Tokens-Total: 23
X-Ratelimit-Remaining: 59

{
  "id": "chatcmpl-DYDw0my3N2EzbjpPCwMrysQ6CZpTm",
  "object": "chat.completion",
  "model": "gpt-4o-mini-2024-07-18",
  "choices": [{
    "index": 0,
    "message": { "role": "assistant", "content": "The capital of France is Paris." },
    "finish_reason": "stop"
  }],
  "usage": { "prompt_tokens": 16, "completion_tokens": 7, "total_tokens": 23 },
  "latency_ms": 4842,
  "cost_usd": 0.0000066
}
```

The **response body follows OpenAI's chat-completion schema** — drop-in for the official OpenAI SDK, LangChain, Vercel AI SDK, or any client that speaks OpenAI. The gateway adds two fields to the body (`latency_ms`, `cost_usd`) and seven `X-*` headers for observability without an SDK:

| Header | What it tells you |
|---|---|
| `X-Provider` | which upstream served this call (`openai` / `anthropic` / `gemini` / `ollama`) — changes when failover kicks in |
| `X-Cache-Hit` | `miss` / `exact` / `semantic` — paired with `X-Cache-Similarity` on semantic hits |
| `X-Cost-Usd` | per-request cost in USD (6 decimals), `0.000000` on cache hits |
| `X-Tokens-*` | token counts normalised across providers (Anthropic, Gemini, Ollama all reported the same way) |
| `X-Ratelimit-Remaining` | tokens left in this API key's bucket, for client-side backpressure |

On a failover, you'll also see `X-Failover: true` and `X-Original-Provider: <name>` — so your dashboards can chart "what got re-routed" without parsing gateway logs.

> The `latency_ms: 4842` above is a **cache miss** — it includes OpenAI's full response time. Run the same request a second time and you'll see `X-Cache-Hit: exact`, `X-Cost-Usd: 0.000000`, and `latency_ms` in the single digits. That's the 3 ms p50 claim from the benchmark table below.

Switch `gpt-4o-mini` for `claude-haiku-4-5-20251001`, `gemini-2.0-flash`, or any local Ollama model (`llama3.3`, `qwen2.5`, `gemma3`, …) — same endpoint, no code changes in your application.

### At a glance

| | |
|---|---|
| **Cache-hit p50 / p99** | **3 ms / 23 ms** on a DigitalOcean 4 vCPU shared Linux droplet ([how it's measured](#performance)) |
| **Rate-limit rejection p50** | **2 ms** — fast-fail protects the gateway from abuse bursts |
| **Throughput** | **~1,672 req/sec** sustained on a DigitalOcean 4 vCPU shared Linux droplet |
| **Semantic caching** | `pgvector` + `all-MiniLM-L6-v2` — catches paraphrased duplicates at `$0` |
| **Binary size / memory** | **30 MB** Go binary, ~50 MB RSS under load |
| **Dependencies** | Postgres + Redis (+ optional bundled embedding service). That's it. |

> **Faster than LiteLLM, self-hosted, and MIT-licensed** — a single 30 MB Go binary with reproducible benchmarks you can run in <5 minutes. See [full benchmark & methodology](#performance).

---

## Why LLM0 Gateway?

- **One endpoint, four backends** — swap between OpenAI, Anthropic, Gemini, and local Ollama models without touching client code.
- **Local-first or cloud-first, your choice** — a single `FAILOVER_MODE` env var decides whether requests try Ollama first, cloud first, local-only, or cloud-only. Great for privacy-sensitive workloads that need cloud as a backup.
- **Never get paged for a provider outage** — automatic failover on `429`/`5xx`/`4xx`/timeout/connection errors across providers. Clients never see the failure.
- **Save real money with two-tier caching** — exact-match cache returns in `<1ms`; an optional **semantic cache** (`pgvector` + `all-MiniLM-L6-v2` embeddings) catches paraphrased duplicates so "What's the capital of France?" and "Tell me France's capital city" share one cached answer. Local Ollama calls cost `$0`.
- **Built-in SaaS controls** — per-API-key rate limits, per-customer spend caps, hard monthly project caps, customer labels for analytics.
- **Zero lock-in** — single Go binary, standard Postgres + Redis, open source.

---

## Features

### Multi-Provider Routing
Route to **OpenAI**, **Anthropic**, **Google Gemini**, and **Ollama** (local models) through a single OpenAI-compatible API. The gateway detects the correct provider from the model name automatically and exposes a standard `GET /v1/models` endpoint for SDK discovery.

### Configurable Failover Modes
Set `FAILOVER_MODE` to control how cloud and local providers are ordered in the failover chain:

| Mode | Behavior | Typical use case |
|---|---|---|
| `cloud_first` *(default)* | Cloud providers first, Ollama as last-resort fallback | Production, best quality + cost reduction |
| `local_first` | Ollama first, cloud as fallback when local fails | Privacy-first apps, air-gapped + cloud-capable |
| `local_only` | Never contact cloud APIs | Offline, compliance, dev without API keys |
| `cloud_only` | Never use local models (even if configured) | Pure cloud deployments |

### Automatic Cross-Provider Failover
When a provider returns `429`, `5xx`, `401`/`403`, `404`, a timeout, or a connection failure, the gateway transparently retries the next provider in the chain — without the caller knowing. Preset chains are defined for all major models.

```
gpt-4o-mini  →  OpenAI (primary)
             →  Anthropic claude-haiku-4-5
             →  Google gemini-2.5-flash
             →  Ollama qwen2.5:14b   (if OLLAMA_BASE_URL is set)
```

Response headers `X-Failover: true` and `X-Original-Provider` tell you when a failover happened.

### Local Ollama Support
Point the gateway at a running Ollama instance (`OLLAMA_BASE_URL=http://host.docker.internal:11434/v1`) and:
- All pulled Ollama models become routable through `/v1/chat/completions`
- They appear automatically in `GET /v1/models`
- Streaming works identically to cloud providers
- Cost is always `$0` — skipped in spend checks and logs
- Tier mapping (`OLLAMA_MODEL_FLAGSHIP`, `_BALANCED`, `_BUDGET`) transparently substitutes local models for cloud equivalents during failover

### Two-Tier Caching — Exact + Semantic

The gateway ships two independent cache layers that stack together to cut LLM spend dramatically:

**1. Exact-match cache** — SHA-256 over `(project_id, model, provider, messages)`. Checked in Redis (`<1ms`) first, falls through to Postgres (`~5ms`) on restart / Redis eviction. Identical requests **never hit the LLM twice**.

**2. Semantic cache** — for when users ask the same thing differently. The first user message is sent to a bundled embedding service, which returns a 384-dim vector. That vector is compared against cached vectors in Postgres using `pgvector` cosine similarity. If the best match exceeds a configurable threshold (default `0.95`), we return that cached response.

```
User A: "What's the capital of France?"         → cache miss, calls OpenAI
User B: "Tell me France's capital city"         → semantic hit (0.97) → $0 instant response
User C: "france capital?"                       → semantic hit (0.96) → $0 instant response
```

Both caches are toggleable per-API-key (`cache_enabled`, `semantic_cache_enabled`) and per-project (`semantic_threshold`). When a semantic hit occurs you get:

- `X-Cache-Hit: semantic`
- `X-Cache-Similarity: 0.973`
- `similarity_score` column populated in `gateway_logs` for offline analysis

### Embedding Service (bundled)

Semantic caching is powered by a small FastAPI service shipped alongside the gateway in `embedding_service/`:

- **Model**: [`all-MiniLM-L6-v2`](https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2) — 22M params, 384-dim output, runs on CPU
- **Runtime**: ~80–150 MB RAM, ~20–40 ms per embedding on a modern CPU
- **Deployment**: included in `docker-compose.yml` as the `embedding` service; model weights are baked into the image at build time so first-request latency is zero
- **Optional**: skip the service entirely and semantic caching disables gracefully — exact-match caching still works
- **Swappable**: implements a simple `POST /embed` contract, so you can point the gateway at any HTTP embedder (BGE, E5, OpenAI `text-embedding-3-small`, self-hosted Instructor) by changing `EMBEDDING_SERVICE_URL`

The architecture is deliberate: keeping embeddings in a separate process means you can scale the embedding service independently, swap in a different model without rebuilding the gateway, or point at a GPU-backed embedder for throughput.

### Streaming (SSE)
Full Server-Sent Events support for **all four providers** (OpenAI, Anthropic, Gemini, Ollama). Chunks are normalized to a single OpenAI-compatible `chat.completion.chunk` shape regardless of which provider is upstream, so the same client code works against any backend.

Send `"stream": true` to get a stream instead of a blocking JSON response:

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llm0_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role":"user","content":"count to 10 slowly"}],
    "stream": true
  }'
```

The response starts with standard OpenAI chunks, ends with a **metadata frame** carrying cost / usage / latency (so you don't need a second call to know what the request cost), and terminates with `[DONE]`:

```
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"delta":{"content":"Sure"}}],...}
data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"delta":{"content":"!"}}],...}
...
data: {"object":"chat.completion.chunk.metadata","usage":{"prompt_tokens":5,"completion_tokens":38,"total_tokens":43},"cost_usd":0.0000236,"latency_ms":1962,"provider":"openai"}
data: [DONE]
```

**Streaming behavior notes:**
- **Ollama empty-chunk filtering (default on).** Ollama's OpenAI adapter often sends many role-only SSE frames before content; the gateway drops those duplicates and keeps the first `role` chunk plus all content/`finish_reason` frames. Set `OLLAMA_FILTER_EMPTY_CHUNKS=false` to pass the raw stream through.
- **Cache hits return a single JSON body, not a stream.** The response is already complete — there's nothing to stream — so you get the cached payload with `X-Cache-Hit: exact` or `semantic` set. Treat `Content-Type: application/json` in response to a stream request as "this was a cache hit." This matches OpenAI's own caching semantics.
- **Failover is disabled for streaming requests.** Once a single chunk has been written to the client, we can't retry against a different provider without breaking the stream. Non-streaming requests keep full automatic failover. If provider reliability matters more than streaming UX, set `"stream": false`.
- **No client-side timeout issues.** The gateway disables the server's 60-second `WriteTimeout` on streaming requests only, so long reasoning outputs (o1, Claude extended thinking) and slow local Ollama generations aren't truncated.
- **Post-stream caching runs in a background goroutine** after `[DONE]`, so the second identical request returns from cache with the full metadata and no LLM call.

### Token Bucket Rate Limiting (per API key)
Each API key has its own `rate_limit_per_minute` enforced atomically in Redis via Lua scripts — no race conditions under high concurrency. Uses a full token bucket algorithm (not a naive counter), so burst traffic within the minute is allowed as long as the per-minute rate isn't breached.

Response headers included on every call:
- `X-RateLimit-Limit`
- `X-RateLimit-Remaining`
- `X-RateLimit-Reset` (Unix timestamp)

When the limit is exceeded, the gateway returns `429` with a `retry_after` field.

### Per-Customer Spend Caps
Pass `X-Customer-ID` on any request to enable per-end-user daily and monthly USD spend limits. Limits are stored in the `customer_limits` table and support two overflow behaviors:
- `block` — return `429` with spend details and how much longer until reset
- `downgrade` — automatically route to a cheaper model (e.g. `gpt-4o` → `gpt-4o-mini`)

Customer labels (`X-LLM0-Tier: pro`, `X-LLM0-Team: billing`, …) are stored as JSONB on every request log for downstream analytics.

### Hard Project Spend Cap
Set `monthly_cap_usd` on a project and requests are blocked with `402 Payment Required` once the cap is hit. Checked **before** the LLM call using cost estimation, so runaway prompts can't silently exceed the cap.

### Cost Tracking
Pre-request cost estimation (for spend cap checks) plus post-request reconciliation based on actual token usage. Costs are pulled from the `model_pricing` table and stored per request. Local Ollama calls are always `$0`.

### Request Logging
Every request is logged to `gateway_logs` with: provider, model, tokens, cost, latency, cache status (exact/semantic/miss), similarity score, failover info, customer ID, and arbitrary labels.

### Background Workers
Runs in-process as Go goroutines — no separate cron container.
- **Monthly spend reset** — zeroes `projects.current_month_spend_usd` at 00:00 UTC on the 1st; catches up on missed resets after downtime
- **Exact cache cleanup** — hourly prune of expired `exact_cache` rows
- **Semantic cache cleanup** — daily at 02:00 UTC, prunes `semantic_cache` rows past their per-row TTL
- **Log maintenance** — weekly `gateway_logs` retention cleanup (Sunday 03:00 UTC)
- **Spend reconciliation** — hourly drift check between Redis counters and Postgres

Every run writes an audit row to `system_logs` (when it does work). Disable all five with `DISABLE_BACKGROUND_WORKERS=true` for multi-replica deployments — enforcement is Redis-authoritative and unaffected. See [Background Worker Schedule](#background-worker-schedule) for the full cadence table and operational notes, and [How Spend Caps Reset](#how-spend-caps-reset) for how these jobs tie into cap enforcement.

---

## Supported Models

Pricing ships pre-seeded in [`schema/seed_models.sql`](schema/seed_models.sql) and can be extended at runtime via [`scripts/manage_models.sh`](scripts/manage_models.sql) — no code changes or redeploy required. New models from any cloud provider are auto-routable as soon as they're added to the pricing table (see [Dynamic Model Routing](#managing-model-pricing)).

### OpenAI
| Model | Tier | Context | Input $/1K | Output $/1K |
|---|---|---:|---:|---:|
| `gpt-5.4` | Flagship | 1M | $0.0025 | $0.0150 |
| `gpt-5.4-mini` | Balanced | 1M | $0.00025 | $0.0020 |
| `gpt-5.4-nano` | Budget | 1M | $0.0001 | $0.0008 |
| `gpt-4o` | Flagship (prev-gen) | 128K | $0.0025 | $0.0100 |
| `gpt-4o-mini` | Cost-optimized | 128K | $0.00015 | $0.0006 |
| `gpt-4-turbo` | Legacy flagship | 128K | $0.0100 | $0.0300 |
| `gpt-3.5-turbo` | Budget | 16K | $0.0005 | $0.0015 |

### Anthropic
| Model | Tier | Context | Input $/1K | Output $/1K |
|---|---|---:|---:|---:|
| `claude-opus-4-7` | Flagship | 200K | $0.0050 | $0.0250 |
| `claude-opus-4-6` | Most capable | 200K | $0.0150 | $0.0750 |
| `claude-sonnet-4-6` | Balanced | 200K | $0.0030 | $0.0150 |
| `claude-opus-4-5-20251101` | Most capable (dated) | 200K | $0.0150 | $0.0750 |
| `claude-sonnet-4-5-20250929` | Balanced (dated) | 200K | $0.0030 | $0.0150 |
| `claude-haiku-4-5-20251001` | Cost-optimized | 200K | $0.0008 | $0.0040 |
| `claude-sonnet-4-20250514` | Balanced (legacy) | 200K | $0.0030 | $0.0150 |
| `claude-3-haiku-20240307` | Budget | 200K | $0.00025 | $0.00125 |

### Google Gemini
| Model | Tier | Context | Input $/1K | Output $/1K |
|---|---|---:|---:|---:|
| `gemini-2.5-pro` | Most capable | 2M | $0.00125 | $0.0100 |
| `gemini-2.5-flash` | Balanced | 1M | $0.0001 | $0.0004 |
| `gemini-2.0-flash` | Cost-optimized | 1M | $0.0001 | $0.0004 |
| `gemini-2.0-flash-lite` | Budget | 1M | $0.000075 | $0.00030 |

> Any new model you add to `model_pricing` is **automatically routable** — the provider is selected by name prefix (`gpt-*` → OpenAI, `claude-*` → Anthropic, `gemini-*` → Google). No code change or redeploy required when a provider ships a new model.

### Ollama (local)
Any model pulled on your Ollama instance is automatically routable — `llama3.3:70b`, `qwen2.5:14b`, `gemma3:4b`, `mistral`, `deepseek-r1`, etc. Pull models with `ollama pull <model>` and they appear in `GET /v1/models` instantly. All Ollama requests are metered at **$0 cost**.

The tier env vars (`OLLAMA_MODEL_FLAGSHIP`, `OLLAMA_MODEL_BALANCED`, `OLLAMA_MODEL_BUDGET`) tell the failover engine which local model to substitute when a cloud model is requested. For example, with `OLLAMA_MODEL_BALANCED=qwen2.5:14b` set, a `gpt-4o-mini` request in `local_first` mode tries `qwen2.5:14b` first, then `gpt-4o-mini` on OpenAI if the local call fails.

---

## Quick Start

### Option A — Docker Compose (recommended)

Requires: [Docker Desktop](https://www.docker.com/products/docker-desktop/).

**Step 1 — Clone and configure**

```bash
git clone https://github.com/mrmushfiq/llm0-gateway
cd llm0-gateway

cp .env.example .env
```

Open `.env` and add at least one provider API key:

```env
OPENAI_API_KEY=sk-proj-...
ANTHROPIC_API_KEY=sk-ant-...
GEMINI_API_KEY=AIza...
```

**Step 2 — Build the images**

```bash
docker compose build
```

> **This takes 3–5 minutes on first run and pulls ~3 GB.** The embedding service is the heavy dependency — it ships PyTorch + sentence-transformers for the `all-MiniLM-L6-v2` model (~90 MB weights) and compresses to a ~3 GB image (~7 GB on disk once all Docker layers are extracted). Subsequent builds use the Docker layer cache and complete in seconds.
>
> **Don't need semantic caching?** Skip the embedding service entirely — see the note under **Step 3** below for the lightweight startup command. Exact-match caching still works without it.

**Step 3 — Start all services**

```bash
docker compose up
```

Postgres (with `pgvector`), Redis, the embedding service, and the gateway all start together. The database schema is applied automatically on first boot.

> **Don't want the ~3 GB embedding service?** Start just the three core containers instead:
>
> ```bash
> docker compose up postgres redis gateway
> ```
>
> Exact-match caching still works without it. To fully disable semantic cache (so the gateway doesn't attempt to reach a service that isn't running), see [Turning Semantic Cache Off](#turning-semantic-cache-off). A cleaner single-flag opt-out is planned for v0.1.3.

When you see:

```
llm0_gateway  | ✅ Failover executor initialized with 3 providers
llm0_gateway  | ✅ Semantic cache initialized
llm0_gateway  | 🚀 LLM0 Gateway listening on :8080
```

the gateway is ready.

**Step 4 — Create an API key**

```bash
./scripts/create_api_key.sh
```

**Step 5 — Send your first request**

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llm0_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Say hello!"}]}'
```

**Useful Docker commands**

```bash
# Run in background
docker compose up -d

# View gateway logs
docker compose logs -f gateway

# Stop everything
docker compose down

# Stop and wipe all data (full reset)
docker compose down -v

# Restart just the gateway (e.g. after editing .env)
docker compose up -d gateway
```

> **Skipping the embedding service** — see the [Turning Semantic Cache Off](#turning-semantic-cache-off) section for the current (manual) procedure. A clean CLI-only opt-out is tracked for v0.1.3.

**Step 6 — (Optional) Add local Ollama models**

If you're running [Ollama](https://ollama.com) on your host machine, point the gateway at it for local, zero-cost inference with cloud failover:

```env
# In .env
OLLAMA_BASE_URL=http://host.docker.internal:11434/v1
FAILOVER_MODE=local_first

# Map local models to tiers (match whatever you've pulled)
OLLAMA_MODEL_FLAGSHIP=llama3.3:70b
OLLAMA_MODEL_BALANCED=qwen2.5:14b
OLLAMA_MODEL_BUDGET=gemma3:4b
```

Then restart the gateway and test:

```bash
docker compose up -d --force-recreate gateway

# Request a cloud model — gets served by Ollama first, cloud as fallback
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llm0_live_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello!"}]}'

# List everything the gateway can route (cloud + local)
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer llm0_live_..."
```

The `X-Provider` response header shows which backend actually served the request.

### Option B — Run with Go

Requires: Go 1.24+, Postgres with the `pgvector` extension, Redis.

**Step 1 — Clone and configure**

```bash
git clone https://github.com/mrmushfiq/llm0-gateway
cd llm0-gateway

cp .env.example .env
# Edit .env — set DATABASE_URL, REDIS_URL, and at least one provider key
```

**Step 2 — Apply the database schema**

```bash
psql $DATABASE_URL -f schema/schema.sql
```

**Step 3 — (Optional) Start the embedding service for semantic caching**

```bash
cd embedding_service
pip install -r requirements.txt
uvicorn app:app --host 0.0.0.0 --port 8001
```

Then set `EMBEDDING_SERVICE_URL=http://localhost:8001` in your `.env`. Skip this step to run without semantic caching — exact-match caching still works.

**Step 4 — Run the gateway**

```bash
go run ./cmd/gateway/main.go
```

Or build a binary:

```bash
go build -o llm0-gateway ./cmd/gateway/main.go
./llm0-gateway
```

---

## Managing Model Pricing

### How the default list is seeded

The gateway ships with a curated set of model prices in [`schema/seed_models.sql`](schema/seed_models.sql). It's applied automatically on **first** Postgres boot via the `docker-entrypoint-initdb.d/` mount.

- **Docker Compose users (fresh install)** — no action needed. Works out of the box.
- **Docker Compose users (existing install)** — Postgres only runs initdb scripts on an empty data volume, so an upgraded `seed_models.sql` won't auto-apply. Re-run it manually against your live DB (safe — idempotent):
  ```bash
  docker compose exec -T postgres psql -U llm0 -d llm0_gateway \
    -f /docker-entrypoint-initdb.d/02_seed_models.sql
  ```
- **Non-Docker / manual Postgres** — after applying `schema/schema.sql`, also run:
  ```bash
  psql $DATABASE_URL -f schema/seed_models.sql
  ```

The seed uses `ON CONFLICT (provider, model) DO NOTHING`, so it's safe to re-run and will never overwrite rows you've managed manually.

> **Want stricter schema versioning?** The project ships a single `schema.sql` + `seed_models.sql` pair for simplicity. If your team prefers versioned, reversible migrations, drop in [`golang-migrate`](https://github.com/golang-migrate/migrate) (classic up/down SQL files) or [Atlas](https://atlasgo.io/) (declarative, diff-based) — both integrate cleanly without changing application code.

### Adding / updating / removing entries

Model prices live in the `model_pricing` table. Use the bundled interactive script to add, update, or delete entries when providers release new models or change prices:

```bash
./scripts/manage_models.sh           # interactive menu
./scripts/manage_models.sh list      # list all models
./scripts/manage_models.sh add       # add a new model
./scripts/manage_models.sh update    # update pricing for an existing model
./scripts/manage_models.sh delete    # remove a model
```

After any change, restart the gateway to reload the pricing cache:

```bash
docker compose restart gateway
```

Prices are specified per 1,000 tokens in USD (e.g. `gpt-4o-mini` input is `0.00015`). Ollama models can be added with `0.00000000` prices to make their cost explicit in request logs.

### Keeping pricing current

Provider pricing drifts — new models launch, old ones get cheaper, and context windows change. Here's the policy:

| Situation | What to do |
|---|---|
| New model released upstream | Add it with `./scripts/manage_models.sh add` — no code change needed. Cloud providers are routed by prefix (`gpt-*`, `claude-*`, `gemini-*`), so new models work immediately. |
| Want the fix to persist across fresh installs | Submit a PR updating [`schema/seed_models.sql`](schema/seed_models.sql). That single file is the canonical source of truth. |
| Pricing changed on an existing model | `./scripts/manage_models.sh update` locally; PR the seed file for the upstream fix. |
| Running a fleet of gateways | Roll out the updated `seed_models.sql` and apply it once per database (`psql ... -f seed_models.sql`). It's idempotent, so re-running is safe. |

We intentionally **do not** auto-scrape provider pricing pages: those pages are unstable, ToS-ambiguous, and silently reformat. Community-reviewed PRs against `seed_models.sql` are the safest long-term update channel — the same approach LiteLLM uses.

---

## Creating Your First API Key

API keys are in the format `llm0_live_<64 hex chars>`. Only the `bcrypt(SHA-256(key))` hash is stored — the raw key is shown once.

The script requires Docker Compose to be running (uses `pgcrypto` inside Postgres — no host dependencies needed):

```bash
./scripts/create_api_key.sh
```

Example output:

```
════════════════════════════════════════════════
  LLM0 Gateway — Create API Key
════════════════════════════════════════════════

▶  Generated key (save this — shown only once):

   llm0_live_c0244eec5b7a8426a6a96b5f9748efa8...

▶  Bcrypt hash generated (via pgcrypto)
▶  Project ID : 54ce26a8-2f93-4afd-924d-28a8832ea52e
▶  Key prefix : llm0_live_c0244...

  Test it:

  curl http://localhost:8080/v1/chat/completions \
    -H "Authorization: Bearer llm0_live_c0244..." \
    ...
════════════════════════════════════════════════
```

---

## Configuration

All configuration is via environment variables. Copy `.env.example` to `.env`.

### Required

| Variable | Description |
|---|---|
| `DATABASE_URL` | Postgres connection string (must have `pgvector` extension) |
| `REDIS_URL` | Redis connection string |
| At least one of: `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `OLLAMA_BASE_URL` | The gateway routes to whichever providers have keys set |

### Cloud Providers

| Variable | Default | Description |
|---|---|---|
| `OPENAI_API_KEY` | — | OpenAI API key |
| `ANTHROPIC_API_KEY` | — | Anthropic API key |
| `GEMINI_API_KEY` | — | Google Gemini API key |

### Local Models (Ollama)

| Variable | Default | Description |
|---|---|---|
| `OLLAMA_BASE_URL` | `""` | Set to enable local models. In Docker: `http://host.docker.internal:11434/v1`. Native: `http://localhost:11434/v1` |
| `OLLAMA_MODEL_FLAGSHIP` | `llama3.3:70b` | Local model used as substitute for flagship-tier cloud models (gpt-4o, claude-opus, gemini-pro) |
| `OLLAMA_MODEL_BALANCED` | `qwen2.5:14b` | Local model used as substitute for balanced-tier cloud models (gpt-4o-mini, claude-sonnet, gemini-flash) |
| `OLLAMA_MODEL_BUDGET` | `gemma3:4b` | Local model used as substitute for budget-tier cloud models (gpt-3.5, claude-haiku, gemini-flash-lite) |
| `OLLAMA_FILTER_EMPTY_CHUNKS` | `true` | Drop empty role-only SSE chunks from Ollama streams before forwarding to clients. Set `false` for raw upstream chunks |

### Failover

| Variable | Default | Description |
|---|---|---|
| `FAILOVER_MODE` | `cloud_first` | One of `cloud_first`, `local_first`, `local_only`, `cloud_only`. See [Failover Modes](#configurable-failover-modes) above |

### Server & Infrastructure

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | Gateway listen port |
| `ENVIRONMENT` | `local` | `local` or `production` (switches Gin to release mode) |
| `CACHE_TTL_SECONDS` | `3600` | Dual-purpose: (1) default TTL for exact-match cache entries (overridable per project via `projects.cache_ttl_seconds`), and (2) TTL for the Redis `apikey:*` auth cache. Config changes to `monthly_cap_usd`, `rate_limit_per_minute`, or cache flags take up to this long to propagate unless you flush `apikey:*` manually. See `design/enforcement-and-caching.md` |
| `CUSTOMER_LIMIT_CACHE_TTL_SECONDS` | `60` | TTL for the in-process `customer_limits` cache (per end-user spend/request caps). Changes to the `customer_limits` table propagate within this window, or immediately when updated through the gateway's own data-access layer |
| `EMBEDDING_SERVICE_URL` | `""` | Enables semantic caching when set. Docker Compose sets this automatically |
| `REQUEST_TIMEOUT` | `30s` | Upstream request timeout |
| `MAX_CONCURRENT_REQUESTS` | `10000` | Concurrency ceiling for the HTTP server |
| `DISABLE_BACKGROUND_WORKERS` | `false` | Skip starting scheduled goroutines (monthly spend reset, cache/log cleanup, reconciliation). Useful in multi-replica deployments where only one replica should run maintenance |

### TLS (optional)

| Variable | Default | Description |
|---|---|---|
| `TLS_ENABLED` | `false` | Enable TLS 1.3 |
| `TLS_CERT_FILE` | — | Path to certificate |
| `TLS_KEY_FILE` | — | Path to private key |

---

## How It Works

### Request Pipeline

```
Incoming Request
        │
        ▼
  Auth Middleware          validate Bearer token (bcrypt verify, Redis-cached)
        │
        ▼
  Rate Limit Check         token bucket per API key via atomic Redis Lua script
        │
        ▼
  Spend Cap Check          block if project monthly_cap_usd exceeded
        │
        ▼
  Exact-Match Cache        SHA-256 key → Redis (<1ms) → Postgres (~5ms)
        │  cache hit: return immediately
        ▼
  Semantic Cache           pgvector cosine similarity search (~20–50ms)
        │  cache hit: return immediately
        ▼
  Customer Limit Check     per X-Customer-ID daily/monthly spend cap
        │
        ▼
  Provider Call            OpenAI / Anthropic / Gemini
        │  on 429/5xx/timeout → automatic failover to next provider
        ▼
  Response                 streaming SSE or non-streaming JSON
        │
        ▼
  Async Workers            log request, update spend counters, store in cache
```

### Failover Chains

Failover chains are **dynamically composed at request time** based on `FAILOVER_MODE` and whether Ollama is configured. The base cloud chains are defined in `internal/gateway/failover/chains.go`.

**Base cloud chains** (used when no Ollama is configured, or in `cloud_only` mode):

| Requested Model | Step 1 | Step 2 | Step 3 |
|---|---|---|---|
| `gpt-4o` | OpenAI | Anthropic claude-sonnet-4-6 | Google gemini-2.5-pro |
| `gpt-4o-mini` | OpenAI | Anthropic claude-haiku-4-5 | Google gemini-2.5-flash |
| `claude-sonnet-4-6` | Anthropic | OpenAI gpt-4o | Google gemini-2.5-pro |
| `claude-haiku-4-5-20251001` | Anthropic | OpenAI gpt-4o-mini | Google gemini-2.5-flash |
| `gemini-2.5-pro` | Google | OpenAI gpt-4o | Anthropic claude-sonnet-4-6 |
| `gemini-2.5-flash` | Google | OpenAI gpt-4o-mini | Anthropic claude-haiku-4-5 |

**Effect of `FAILOVER_MODE`** (example: request for `gpt-4o-mini` with `OLLAMA_MODEL_BALANCED=qwen2.5:14b`):

| Mode | Resulting chain |
|---|---|
| `cloud_only` | OpenAI → Anthropic haiku → Gemini flash |
| `cloud_first` | OpenAI → Anthropic haiku → Gemini flash → Ollama qwen2.5:14b |
| `local_first` | Ollama qwen2.5:14b → OpenAI → Anthropic haiku → Gemini flash |
| `local_only` | Ollama qwen2.5:14b |

**Tier resolution** — the gateway chooses which Ollama model to substitute based on the cloud model's quality tier: flagship (gpt-4o, claude-opus, gemini-pro), balanced (gpt-4o-mini, claude-sonnet, gemini-flash), or budget (gpt-3.5, claude-haiku, gemini-flash-lite).

**Failover triggers**: `429` (rate limit), `5xx` (server error), connection timeout, connection error, `401`/`403` (auth failure — next provider may have a valid key), `404` (model not available on that provider).

### Exact-Match Cache

Cache key: `SHA-256(project_id + provider + model + sorted_messages_json)`

Two-tier lookup:
1. **Redis** (hot) — sub-millisecond, in-memory
2. **Postgres** (warm) — ~5ms, survives Redis restarts

Cache hits cost `$0.00` and are returned in `<1ms`.

### Semantic Cache

When `EMBEDDING_SERVICE_URL` is configured, the first user message is embedded using `all-MiniLM-L6-v2` (384 dimensions). The embedding is compared against stored vectors in Postgres using `pgvector` cosine similarity.

```
Gateway ──POST /embed──► Embedding Service (all-MiniLM-L6-v2, CPU)
        ◄─[0.12, -0.34, ...]──

        ──cosine similarity──► pgvector (threshold: 0.95)
```

Cache hits return the stored response without any LLM API call.

**Threshold**: configurable per project (`semantic_threshold` column, default `0.95`). Lower values return more matches but risk returning less relevant cached responses.

### Turning Semantic Cache Off

There are two ways to disable semantic caching, depending on scope:

**1. Globally (all projects)** — unset `EMBEDDING_SERVICE_URL` in your environment. The gateway logs `⚠️ Semantic cache disabled (no EMBEDDING_SERVICE_URL)` at startup and skips the semantic lookup entirely. Exact-match caching is unaffected. The `embedding` service in `docker-compose.yml` can be removed or left idle — it's never called.

```bash
# In .env
EMBEDDING_SERVICE_URL=

# Or stop the embedding container alone
docker compose stop embedding
```

**2. Per project** — flip the `semantic_cache_enabled` column on the `projects` table. API keys inherit their project's setting, so every key scoped to that project loses semantic cache on the next auth cache refresh (up to `CACHE_TTL_SECONDS`, default **1 hour**). To force immediate pickup, flush the cached API-key blobs:

```bash
docker compose exec redis redis-cli --scan --pattern "apikey:*" | \
  xargs -r docker compose exec -T redis redis-cli DEL
```

```bash
./scripts/manage_limits.sh           # menu option 6 — "Update project cache settings"
```

Or by SQL:

```sql
UPDATE projects
SET semantic_cache_enabled = false
WHERE id = '<project-uuid>';
```

Use per-project disable when you have mixed workloads — e.g., chat UIs benefit from semantic hits, but tool-calling agents need exact matches because a single token difference changes intent. The `cache_enabled` column on the same table toggles the exact-match cache independently, so you can keep one and disable the other.

**Note on existing cache rows** — disabling semantic cache only stops *reads and writes*; rows already in the `semantic_cache` table stay put. They'll age out naturally via the daily cleanup job (see below), or you can clear them manually:

```sql
DELETE FROM semantic_cache WHERE project_id = '<project-uuid>';
```

---

## Response Headers

Every response includes diagnostic headers:

| Header | Description |
|---|---|
| `X-Cache-Hit` | `exact`, `semantic`, or `miss` |
| `X-Cache-Similarity` | Cosine similarity score (semantic hits only) |
| `X-Provider` | Which provider served the response |
| `X-Cost-USD` | Actual cost of the request |
| `X-Tokens-Prompt` | Prompt token count |
| `X-Tokens-Completion` | Completion token count |
| `X-RateLimit-Remaining` | Requests remaining in current window |
| `X-Failover` | `true` if failover occurred |
| `X-Original-Provider` | Provider that was tried first (on failover) |

---

## Rate Limiting & Cost Controls

The gateway has **three independent layers** of usage control, evaluated in order on every request.

> **TL;DR — tune everything via an interactive CLI:**
>
> ```bash
> ./scripts/manage_limits.sh
> ```
>
> The script wraps `psql` with a menu-driven UI for updating API-key rate limits, project spend caps, cache/semantic settings, and per-customer limits without writing SQL. Changes take effect without a gateway restart.

### 1. Per-API-Key Rate Limit (requests/minute)
A token-bucket algorithm runs atomically in Redis via a Lua script — no race conditions even under thousands of concurrent calls. Each API key has its own `rate_limit_per_minute` stored in the `api_keys` table.

```bash
# Interactive (recommended)
./scripts/manage_limits.sh set-key-rate

# Or direct SQL
docker compose exec postgres psql -U llm0 -d llm0_gateway -c \
  "UPDATE api_keys SET rate_limit_per_minute = 120 WHERE key_prefix = 'llm0_live_abc12';"
```

The client sees:
- `X-RateLimit-Limit` — the bucket capacity
- `X-RateLimit-Remaining` — tokens left in the current window
- `X-RateLimit-Reset` — Unix timestamp when the bucket refills
- `429` with `retry_after` when exceeded

### 2. Hard Project Spend Cap (USD/month)
Each project has a `monthly_cap_usd` column. The gateway **estimates** the request cost before calling the LLM; if it would push the project over the cap, the request is blocked with `402 Payment Required`. This prevents runaway prompts from silently burning dollars.

```bash
./scripts/manage_limits.sh set-project-cap
```

### 3. Per-Customer Spend Limits (daily + monthly USD)
Set limits per end-user via the `customer_limits` table. The interactive script handles upsert logic, validation, and NULL handling for you:

```bash
./scripts/manage_limits.sh set-customer-limit
```

Or directly:

```sql
INSERT INTO customer_limits (
    project_id, customer_id,
    daily_spend_limit_usd, monthly_spend_limit_usd,
    on_limit_behavior, downgrade_model
) VALUES (
    '<your-project-id>',
    'user_123',
    1.00,          -- $1 per day
    20.00,         -- $20 per month
    'downgrade',   -- 'block' or 'downgrade'
    'gpt-4o-mini'  -- used when on_limit_behavior = 'downgrade'
);
```

Then pass the customer ID on requests:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llm0_live_..." \
  -H "X-Customer-ID: user_123" \
  ...
```

Spend headers are included in every response:
- `X-Customer-Spend-Today`
- `X-Customer-Limit-Daily`
- `X-Customer-Remaining-Usd`

### How Spend Caps Reset

All three spend counters (project `monthly_cap_usd`, customer daily, customer monthly) reset automatically — you don't run a cron job.

**1. Redis is the source of truth for enforcement.**
Every request calls into a Lua script that reads and increments counters stored under date-stamped keys:

| Counter | Redis key | Rotation |
|---|---|---|
| Project monthly spend | `spend:project:{project_id}:{YYYY-MM}` | New key on 1st of each month |
| Customer daily spend | `spend:customer:{project_id}:{customer_id}:daily:{YYYY-MM-DD}` | New key at UTC midnight |
| Customer monthly spend | `spend:customer:{project_id}:{customer_id}:monthly:{YYYY-MM}` | New key on 1st of each month |

When the date rolls over, the Lua script computes a new key name and starts fresh at `$0.00`. The old keys are still in Redis but no longer read — they're garbage-collected by a `TTL` set on every write (31 days for monthly keys, 24 hours for daily keys). **No manual intervention, no cron job, no downtime window.**

**2. Postgres mirrors for reporting.**
The `projects.current_month_spend_usd` column and the `customer_spend` rows exist so you can run SQL dashboards. They're maintained by an async write path (off the hot request path) and reset/pruned by a goroutine scheduler:

- `resetMonthlySpend` runs at 00:00 UTC on the 1st of each month, setting `projects.current_month_spend_usd = 0` and advancing `spend_reset_at` to the next month's 1st. If the gateway was down on the 1st, the next startup catches up via `WHERE spend_reset_at <= NOW()`.
- `cleanupExpiredCache` and `cleanupSemanticCache` prune stale cache rows hourly and daily.
- `cleanupOldLogs` runs weekly (Sunday 03:00 UTC) to trim `gateway_logs` retention.
- `reconcileCustomerSpend` runs hourly to detect drift between Redis and Postgres customer-spend totals (for observability only — Redis remains authoritative).

All five workers are started from `cmd/gateway/main.go` on boot and cancelled on `SIGINT`/`SIGTERM`. Set `DISABLE_BACKGROUND_WORKERS=true` in multi-replica deployments where only one replica should run maintenance, or in tests.

**3. Redis persistence matters for production.**
Because enforcement reads Redis counters directly, Redis restarts without AOF/RDB persistence will reset spend counters mid-month. The bundled `docker-compose.yml` enables `appendonly yes`; verify the same in any managed Redis you use. If you lose Redis data, the `reconcileCustomerSpend` job will flag the drift on its next run — rebuild counters from `SELECT SUM(cost_usd) FROM gateway_logs WHERE project_id = ... AND created_at >= date_trunc('month', NOW())` if needed.

**Manually overriding a reset or unblocking a customer:**

```bash
# Bump a project's monthly cap (immediately picked up — no gateway restart)
./scripts/manage_limits.sh set-project-cap

# Raise a specific customer's daily or monthly limit
./scripts/manage_limits.sh set-customer-limit

# Nuclear option: zero out the Redis counter for a project mid-month
docker compose exec redis redis-cli DEL "spend:project:<project_id>:$(date -u +%Y-%m)"
```

### Background Worker Schedule

All scheduled jobs run as in-process Go goroutines — no cron, no sidecar container, no external dependency. On startup the gateway logs each job's next-run time, e.g.:

```
⏰ [spend-reset] Next run in 258h2m43s
⏰ [semantic-cache-cleanup] Next run in 20h2m43s
⏰ [cache-cleanup] Scheduled hourly, first run in 2m43s
```

| Job | Cadence | Touches | `system_logs.event_type` |
|---|---|---|---|
| `cache-cleanup` | Hourly | `DELETE FROM exact_cache WHERE expires_at < NOW()` | `cache_cleanup` (only if >100 rows) |
| `semantic-cache-cleanup` | Daily at **02:00 UTC** | `DELETE FROM semantic_cache WHERE created_at + (ttl_seconds ‖ 'seconds')::interval < NOW()` | `semantic_cache_cleanup` (only if >100 rows) |
| `log-cleanup` | Weekly, Sunday at **03:00 UTC** | Trims `gateway_logs` per retention policy | `log_cleanup` |
| `reconciliation` | Hourly | Read-only drift check: Redis `spend:customer:…` vs `customer_spend` table | `customer_spend_reconciliation` |
| `spend-reset` | Monthly, day 1 at **00:00 UTC** | Zeroes `projects.current_month_spend_usd`; advances `spend_reset_at` | `monthly_spend_reset` |

**Why these specific cadences:**

- **Exact-match cache is pruned hourly** because it churns fast (`CACHE_TTL_SECONDS` defaults to 1 hour), and row count grows linearly with traffic.
- **Semantic cache is pruned daily at 02:00 UTC** because rows live longer (per-row `ttl_seconds`, typically hours to days), the `pgvector` HNSW index makes deletes more expensive than a plain b-tree, and scheduling off-peak avoids contention with business-hours traffic.
- **Log cleanup is weekly** because `gateway_logs` is the most write-heavy table and clients frequently query it for dashboards; running daily would add vacuum pressure.
- **Reconciliation is hourly** because it's read-only and cheap — it just compares key counts between Redis and Postgres so you catch drift early.
- **Spend reset is monthly** on the 1st at 00:00 UTC because that's when new date-stamped Redis keys start being used; Postgres just needs to mirror the rollover.

**Operational notes:**

- **Audit trail** — cleanup jobs only write to `system_logs` when they actually delete something substantial (>100 rows), to keep the audit table from filling with no-op entries. `spend-reset` and `reconciliation` always write a row.
- **Postgres autovacuum** — `DELETE` marks rows dead but doesn't reclaim space until autovacuum runs. If you do heavy semantic-cache churn (millions of rows/day), schedule a weekly `VACUUM (VERBOSE, ANALYZE) semantic_cache;` outside peak hours.
- **Catch-up on missed runs** — `spend-reset` uses `WHERE spend_reset_at <= NOW()`, so if the gateway was down on the 1st it catches up at next startup. Cache cleanup is self-healing (rows are date-filtered in `expires_at`, so a missed run just means the next one deletes more).
- **Disable for multi-replica** — set `DISABLE_BACKGROUND_WORKERS=true` on all replicas except one dedicated maintenance replica. Enforcement (rate limits, spend caps) is unaffected because it reads directly from Redis; only the Postgres reporting/cleanup layer goes dormant. Startup log confirms: `⚠️ Background workers disabled via DISABLE_BACKGROUND_WORKERS=true`.

### How Cost is Calculated

The gateway tracks cost in two places: **before** the call (for spend-cap enforcement) and **after** the call (for actual billing).

**1. Pricing source** — the `model_pricing` table, one row per `(provider, model)` pair with `input_per_1k_tokens` and `output_per_1k_tokens`. Pricing is loaded into memory at startup — restart the gateway after updates via `./scripts/manage_models.sh`.

**2. Cost formula** — applied identically in every path:

```
cost_usd = (input_tokens  / 1000) × input_per_1k_tokens
         + (output_tokens / 1000) × output_per_1k_tokens
```

Both input and output prices are always applied. Ollama (local) requests are always `$0`, regardless of token counts.

**3. Pre-request estimation** — used to block requests that would breach a project or customer spend cap *before* any API call is made:

- **Input tokens** are estimated as `sum(len(role) + len(content) + 4) / 4` across all messages (the industry-standard "~4 chars per token" heuristic).
- **Output tokens** use the client-supplied `max_tokens` if present. If not, defaults to `2 × input_tokens` clamped to `[100, 2000]` so neither tiny nor huge prompts produce wildly skewed estimates.

This means clients can send `max_tokens: 500` to get a tight, accurate pre-estimate — useful when hovering near a spend cap.

**4. Post-request actual cost** — the gateway reads real `prompt_tokens` and `completion_tokens` from the provider's response and recalculates, then reconciles the difference against Redis spend counters. Every request log in `gateway_logs` has the real cost.

### Cost Tracking Example

```bash
# Make a request
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llm0_live_..." \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "What is F1?"}],
    "max_tokens": 200
  }'
```

The response headers tell you:
- `X-Cost-USD: 0.000110`
- `X-Tokens-Prompt: 15`
- `X-Tokens-Completion: 180`

Aggregate spend by customer, model, or day:

```sql
-- Top 10 costliest customers this month
SELECT customer_id, SUM(cost_usd) AS total, COUNT(*) AS requests
FROM gateway_logs
WHERE created_at >= date_trunc('month', NOW())
GROUP BY customer_id
ORDER BY total DESC
LIMIT 10;

-- Spend breakdown by model
SELECT model, SUM(cost_usd) AS total, SUM(tokens_total) AS tokens
FROM gateway_logs
WHERE created_at >= NOW() - INTERVAL '7 days'
GROUP BY model
ORDER BY total DESC;

-- Average cost per request by customer tier (from labels)
SELECT labels->>'Tier' AS tier, AVG(cost_usd) AS avg_cost
FROM gateway_logs
WHERE labels ? 'Tier'
GROUP BY tier;
```

### Customer Labels
Attach arbitrary labels to any request for analytics — they're stored as JSONB on `gateway_logs`:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "X-Customer-ID: user_123" \
  -H "X-LLM0-Tier: pro" \
  -H "X-LLM0-Team: billing" \
  ...
```

Query the logs later:
```sql
SELECT labels->>'Tier', SUM(cost_usd) FROM gateway_logs GROUP BY 1;
```

---

## Performance

All numbers are **in-process latency** (`gateway_logs.latency_ms`) — the time from request arrival at the Go handler to the response being written. Excludes client network.

### Test setup

All numbers below come from runs of [`bench/load_test.sh`](bench/load_test.sh):

| Parameter | Value |
|---|---|
| Load tool | [`hey`](https://github.com/rakyll/hey) |
| Concurrency | **20** in-flight workers |
| Total requests | **200** (majority succeed as cache hits, remainder rate-limited by the test key's 60 req/min cap) |
| Payload | `gpt-4o-mini` chat completion, 1 user message, ~40 tokens total |
| Measurement source | `gateway_logs.latency_ms` (server-side, excludes client RTT) |

To reproduce:

```bash
docker compose up -d postgres redis
go run ./cmd/gateway &
export LLM0_API_KEY=llm0_live_<your key>
./bench/load_test.sh
```

> The 60 req/min cap on the default test API key is why you'll see 429s — bump `token_bucket_capacity` / `token_bucket_refill_per_min` on the key via `psql` or the management scripts if you want a longer clean run.

### Cache-hit latency across deployments

| Deployment | p50 | p95 | p99 | Throughput (client) | n |
|---|---:|---:|---:|---:|---:|
| **DO 4 vCPU / 8 GB droplet**, Linux | **3 ms** | **12 ms** | **23 ms** | **~1,672 req/s** | 79 |
| DO 2 vCPU / 2 GB droplet, Linux | 7 ms | 17 ms | 22 ms | ~1,194 req/s | 82 |
| MacBook Air M4, native Go + Docker Desktop (Redis/Postgres) | 11 ms | 15 ms | 16 ms | ~1,480 req/s | 67 |

**The 4 vCPU droplet is faster than the MacBook Air at p50** — not because the droplet CPU is faster (it isn't), but because **Docker Desktop on macOS adds network-VM overhead that Linux containers don't have**. Every Redis round trip on macOS goes through a virtual network bridge into the Docker-for-Mac VM; on Linux the overhead is ~0.05ms. When you're measuring 3ms of gateway work, a 1–2ms network tax per Redis call is half your budget.

This is the real answer to "what is the gateway actually doing?" — auth + rate-limit Lua + cache GET + JSON marshal + response write, all in ~3 ms of CPU when the platform isn't in the way.

### Fast-fail on rejected requests

The gateway is designed to say "no" quickly — rejections short-circuit before the cache lookup, provider routing, and response marshaling:

| Response | p50 | p95 | Path |
|---|---:|---:|---|
| **429 rate-limited** | **~2 ms** | **~6 ms** | auth → Redis Lua token-bucket → 429 |
| 200 cache hit | 3 ms | 12 ms | auth → Redis Lua → Redis GET → marshal → 200 |

Rejections short-circuiting at this speed is the property that keeps a single gateway instance stable during abuse bursts — a runaway client or credential leak can't meaningfully consume gateway CPU because each `DENY` takes ~2 ms of work and 0 provider cost.

### Caveats worth reading before you quote these numbers

- **Sample size is small.** ~80 samples per run for p99 is enough to be directionally right, not tight enough to publish ±0.5 ms. Repeat runs on the same droplet move p99 by ±5–10 ms even with identical script and concurrency — quote a range, not a single point.
- **p99 is GC- and connection-warm-up-bound, not CPU-bound.** The 2 vCPU and 4 vCPU droplets have similar p99s (22–23 ms) because the tail is dominated by Go GC pauses and first-request Redis connection setup, neither of which scale with CPU count. Throwing more hardware at the gateway won't reliably push p99 below ~15 ms without GC tuning (`GOGC=200+`) and pool pre-warming — both out of scope for v0.1.x.
- **These are cache-hit numbers.** Cache misses are dominated by upstream provider latency (`gpt-4o-mini` ≈ 300–800 ms to OpenAI, ≈ 200–500 ms to Anthropic). That's not gateway overhead — that's your LLM taking its time.

### Querying your own percentiles (recommended)

**Always quote the server-side `gateway_logs.latency_ms` numbers — not `hey`'s client-side summary.** `hey` measures end-to-end wall clock on the load generator's machine, which includes:

- Local loopback / network stack latency (small on Linux, 1–2 ms on Docker-for-Mac)
- `hey`'s own goroutine scheduling and HTTP client overhead
- TCP connection reuse state across 20 concurrent workers
- 200s and 429s mixed into one histogram (429s drag the percentiles down)

`gateway_logs.latency_ms` captures only the gateway's own handler time — from request arrival at the Go handler to response being written. That is what you want to advertise as "gateway overhead."

Run this after your benchmark to get per-status-code server-side percentiles:

```bash
docker compose exec -T postgres psql -U llm0 -d llm0_gateway -c "
SELECT status,
       cache_hit,
       count(*)                                                        AS n,
       percentile_disc(0.5)  WITHIN GROUP (ORDER BY latency_ms)        AS p50,
       percentile_disc(0.95) WITHIN GROUP (ORDER BY latency_ms)        AS p95,
       percentile_disc(0.99) WITHIN GROUP (ORDER BY latency_ms)        AS p99
FROM gateway_logs
WHERE created_at > now() - interval '15 minutes'
GROUP BY status, cache_hit;"
```

Example output (4 vCPU / 8 GB DigitalOcean droplet, immediately after `./bench/load_test.sh`):

```
 status  | cache_hit | n  | p50 | p95  | p99
---------+-----------+----+-----+------+------
 success | f         |  6 | 826 | 1856 | 1856
 success | t         | 78 |   4 |   12 |   16
```

- `cache_hit = t` → 78 cache-hit responses with **p50 4 ms, p99 16 ms** of gateway overhead
- `cache_hit = f` → 6 cache-miss responses dominated by OpenAI provider latency (`gpt-4o-mini` was slow that run; this varies 5× day-to-day based on provider load)

### Why `hey`'s numbers are larger than these

Same benchmark, side by side on that run:

| Metric | `hey` (client-side) | `gateway_logs` (server-side) |
|---|---:|---:|
| p50 | 4.5 ms | 4 ms |
| p95 | 14.5 ms | 12 ms |
| p99 | 20 ms | 16 ms |

The client-side numbers are systematically **0.5–5 ms larger** because they include local network stack, `hey` scheduling, and connection setup. On Docker-for-Mac the delta is much larger (10–50+ ms at the tail). Always quote the `gateway_logs` numbers for "what the gateway is actually doing."

### What's in each latency bucket

A p50 of 3 ms on a cache hit covers:

- Bearer-token auth (Redis cache ~0.3 ms)
- API-key token-bucket rate limit (Redis Lua `EVALSHA`, 1 round trip)
- Exact-match cache lookup (Redis `GET`, 1 round trip)
- JSON marshal + HTTP response write
- Gin middleware chain + logging goroutine spawn

For cache misses, add the provider round-trip on top (`gpt-4o-mini` ≈ 300–800 ms to OpenAI, ≈ 200–500 ms to Anthropic).

### A note on Docker Desktop vs production Linux

The laptop row in the table above (11 ms p50) is slower than the **DigitalOcean 4 vCPU shared Linux droplet** (3 ms p50) — not because the droplet has a better CPU, but because **Docker Desktop on macOS routes container traffic through a virtual network bridge into a Linux VM**. Every Redis round trip pays a ~1–2 ms tax on macOS that doesn't exist on native Linux.

Takeaway: **production numbers match the DigitalOcean rows, not the laptop row**. If you're benchmarking the gateway on a Mac and seeing single-digit millisecond p50, that's actually *slower* than what you'll see on a Linux VPS at the same CPU count. Run the benchmark on a real Linux host (EC2, Hetzner, DigitalOcean, Linode, bare metal) for representative numbers before making production decisions.

### Memory footprint

The single Go binary is ~30MB RSS at idle, ~50–80MB under load. Concurrent request capacity is bounded by `MAX_CONCURRENT_REQUESTS` (default 10,000).

---

## Endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | Bearer token | Chat completions — streaming and non-streaming |
| `GET` | `/v1/models` | Bearer token | OpenAI-compatible model list (includes cloud + pulled Ollama models) |
| `GET` | `/health` | None | Basic liveness check |
| `GET` | `/ready` | None | Readiness check (Postgres + Redis connectivity) |
| `GET` | `/live` | None | Liveness check |

---

## Project Structure

```
llm0-gateway/
├── cmd/gateway/main.go              # Entry point, router setup, worker initialization
├── internal/
│   ├── gateway/
│   │   ├── auth/                   # API key validation (bcrypt + Redis cache)
│   │   ├── cache/                  # Exact-match (Redis+Postgres) and semantic cache
│   │   ├── cost/                   # Pre/post request cost calculation
│   │   ├── embeddings/             # HTTP client for embedding service
│   │   ├── failover/               # Failover executor + preset model chains
│   │   ├── handlers/               # Gin HTTP handlers (chat, streaming, health)
│   │   ├── providers/              # OpenAI, Anthropic, Gemini provider clients
│   │   ├── ratelimit/              # Per-API-key and per-customer rate limiting
│   │   ├── streaming/              # SSE normalization across providers
│   │   └── workers/                # Background jobs (cache GC, reconciliation)
│   └── shared/
│       ├── config/                 # Environment variable loader
│       ├── database/               # Postgres connection pool + query helpers
│       ├── models/                 # Shared Go structs (Project, APIKey, etc.)
│       ├── redis/                  # Redis client with rate limit + spend cap logic
│       └── tls/                    # TLS 1.3 config
├── embedding_service/
│   ├── app.py                      # FastAPI embedding server
│   ├── requirements.txt
│   └── Dockerfile                  # Bakes all-MiniLM-L6-v2 weights at build time
├── schema/schema.sql               # Canonical DB schema (single source of truth)
├── scripts/
│   └── create_api_key.sh           # Project + API key creation helper
├── docker-compose.yml              # Postgres, Redis, embedding service, gateway
├── Dockerfile
└── .env.example
```

---

## Architecture

```
                        ┌─────────────────────────────┐
                        │         LLM0 Gateway        │
                        │         (Go, :8080)         │
                        └──────────────┬──────────────┘
                                       │
               ┌───────────────────────┼───────────────────────┐
               │                       │                       │
               ▼                       ▼                       ▼
       ┌──────────────┐      ┌──────────────────┐    ┌──────────────────┐
       │    Redis     │      │    PostgreSQL    │    │ Embedding Service│
       │  Rate limits │      │  API keys, logs  │    │ all-MiniLM-L6-v2 │
       │  Exact cache │      │  Exact cache     │    │   (Python)       │
       │  Spend totals│      │  Semantic cache  │    └──────────────────┘
       └──────────────┘      │  Model pricing   │
                             └──────────────────┘
                                       │
         ┌─────────────────┬───────────┴──────────────┬─────────────────┐
         │                 │                          │                 │
         ▼                 ▼                          ▼                 ▼
 ┌──────────────┐  ┌──────────────┐          ┌──────────────┐  ┌──────────────┐
 │    OpenAI    │  │   Anthropic  │          │ Google Gemini│  │    Ollama    │
 │              │  │              │          │              │  │   (local)    │
 └──────────────┘  └──────────────┘          └──────────────┘  └──────────────┘
                  ◄── cloud providers ──►                      ◄── optional ──►
```

---

## Contributing

Contributions are welcome. Please open an issue before submitting large changes.

Areas where contributions are especially useful:
- Additional provider support (AWS Bedrock, Azure OpenAI, Mistral La Plateforme, Cohere, Groq)
- Admin REST API for key/project/limit management
- Prometheus metrics endpoint (`/metrics`)
- Additional embedding models for semantic cache
- Per-model-class routing rules (e.g. "always route coding tasks to X")

See [`CHANGELOG.md`](./CHANGELOG.md) for what shipped in the current release
(v0.1.1) and what's planned for the next patch (v0.1.2).

---

## License

MIT — see [LICENSE](LICENSE).
