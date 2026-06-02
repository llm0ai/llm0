# Changelog

All notable changes to llm0-gateway are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **A note on versioning.** The first public tag was briefly pushed as
> `v1.0.0` in error, then withdrawn. There is no `v1.0.0` release of this
> project. **`v0.1.1` is the first public release.** Versions before 1.0
> reflect the project's pre-stable status — the HTTP surface is intended
> to stay OpenAI-compatible, but operational semantics (schema, env vars,
> worker cadence) may shift in patch releases until 1.0.

---

## [0.1.1] — 2026-02-11

**First public release.** An OpenAI-compatible LLM gateway with automatic
failover, two-tier caching, streaming, per-customer spend caps, and
scheduled maintenance workers. Runs locally via Docker Compose or `go run`
and fronts four providers (OpenAI, Anthropic, Gemini, local Ollama) behind
a single `/v1/chat/completions` endpoint.

### Added

- **Four providers** behind a single OpenAI-compatible endpoint: OpenAI,
  Anthropic, Gemini, and local Ollama. Routing is prefix-based on model
  name (`gpt-*`, `claude-*`, `gemini-*`, anything else → Ollama).
- **Automatic cross-provider failover** on 429 / 5xx / 404 / timeout /
  network error. Configurable via `FAILOVER_MODE` (`cloud_first`,
  `local_first`, `cloud_only`, `local_only`) with tier-based Ollama
  model mapping.
- **Streaming (SSE)** across all four providers, normalized to
  OpenAI-compatible chunks. Trailing metadata frame carries `cost_usd`,
  `usage`, `latency_ms`, and `provider` before `[DONE]`. Server
  `WriteTimeout` is disabled per-request on streaming endpoints so long
  generations (o1, Claude extended thinking, Ollama on CPU) aren't
  truncated.
- **Exact-match cache** — SHA-256 prompt hash, two-tier Redis (hot) +
  Postgres (warm), configurable TTL. Toggleable per API key.
- **Semantic cache** — pgvector cosine similarity against a bundled
  `all-MiniLM-L6-v2` embedding sidecar. Paraphrased queries hit at 0.954
  similarity in ~41 ms, `$0` cost. Toggleable per project via
  `semantic_cache_enabled`, globally disabled by leaving
  `EMBEDDING_SERVICE_URL` unset.
- **Token-bucket rate limiting** per API key, atomic Redis via Lua.
- **Per-customer spend caps** (daily/monthly USD) with `block` or
  `downgrade` overflow behavior. Per-project hard `monthly_cap_usd`.
- **Cost tracking** — pre-request estimation for cap enforcement plus
  post-request reconciliation against actual token usage. Ollama is
  always `$0`.
- **Request logging** — every call logged to `gateway_logs` with
  provider, model, tokens, cost, latency, cache status, similarity,
  failover path, customer ID, and arbitrary `X-LLM0-*` labels as JSONB.
- **Scheduled maintenance workers** (in-process Go goroutines) —
  monthly spend reset, hourly exact-cache cleanup, daily semantic-cache
  cleanup, weekly log-retention cleanup, hourly Redis/Postgres spend
  reconciliation. Wired into `cmd/gateway/main.go` with a cancellable
  root context for clean shutdown on `SIGINT` / `SIGTERM`.
- **`system_logs` audit table** — written by the scheduler on notable
  runs (cleanup jobs only log when >100 rows affected; `spend-reset`
  and `log-cleanup` always log).
- **`DISABLE_BACKGROUND_WORKERS`** environment variable (default
  `false`) for multi-replica deployments where only one replica should
  run scheduled jobs.
- **Model management CLI** (`scripts/manage_models.sh`) for CRUD on the
  `model_pricing` table without writing raw SQL.
- **Limit management CLI** (`scripts/manage_limits.sh`) — interactive
  menu for API-key rate limits, project spend caps, cache settings, and
  per-customer limits.
- **Database seeding** via `schema/seed_models.sql` loaded through
  `docker-entrypoint-initdb.d/`.
- **GitHub Actions CI** — build, vet, test on every push.
- `GET /v1/models` endpoint returning all configured cloud + local
  models.

### Docs

- Comprehensive `README.md` covering setup, features, rate limiting,
  performance, architecture, and response headers.
- **"How Spend Caps Reset"** section explaining Redis key rotation,
  scheduled jobs, Redis persistence caveats, and manual override
  commands.
- **"Background Worker Schedule"** reference table for all five jobs.
- **"Turning Semantic Cache Off"** section covering global and
  per-project disable paths.
- Companion [`design/`](../design/) directory at the repo root with
  deeper writeups on enforcement (Redis vs Postgres) and the background
  workers subsystem.

### Performance

Measured via `hey` against a native-Go gateway with Redis 7 + Postgres 17
in Docker on an Apple M4 MacBook Air. 200-request run at concurrency 20,
split into 67 cache-hit 200s and 133 rate-limit 429s (test key capped at
60 req/min). Numbers are in-process latency from `gateway_logs.latency_ms`,
excluding client RTT:

| Response | p50 | p95 | p99 |
|---|---:|---:|---:|
| 200 — exact-match cache hit | **11 ms** | 15 ms | 16 ms |
| 429 — rate-limit rejection  | **2.1 ms** | 5.6 ms | 5.6 ms |

Throughput: ~**1,480 req/sec** sustained (client-side, mixed 200 + 429).

On Linux hosts the container-network penalty is ~0.05 ms rather than
Docker-for-Mac's ~5 ms, so production numbers on EC2 / bare metal / k8s
tend to run lower than these. See `README.md` → "Performance" for the
full methodology, query, and Linux-vs-macOS comparison.

---

## [Unreleased]

### Added

- **Project default customer limits** — eight new `default_*` columns on
  the `projects` table (`default_daily_spend_limit_usd`,
  `default_monthly_spend_limit_usd`, `default_per_request_max_usd`,
  `default_requests_per_{minute,hour,day}`, `default_on_limit_behavior`,
  `default_downgrade_model`). Apply to every end-user in the project
  without an `INSERT` into `customer_limits`. Managed via the new
  `scripts/manage_project_defaults.sh` (interactive list/set/clear).
- **Customer tiers** — new `customer_tiers` table holds owner-defined
  plans (`free`, `pro`, `enterprise`, any string slug). Customers attach
  to a tier via the **`X-Customer-Tier`** request header (server-to-server
  trust input). Tier limits override project defaults. Managed via the
  new `scripts/manage_tiers.sh` (list/create/update/delete with UPSERT).
- **Resolver precedence on every request:** tier → project default →
  per-customer override → unlimited. Unknown tier slugs silently fall
  through to the default (typo-resistant). See
  `internal/shared/database/resolver.go` and the new
  `customer_tiers_cache.go` (~60s in-process TTL).
- **Customer auto-provisioning** — `customer_spend` rows are upserted on
  the first request carrying a given `X-Customer-ID`. SaaS owners never
  `INSERT` customers by hand.
- **`LimitSpec` shared cap type** — single struct embedded in both
  `CustomerLimit` and `CustomerTier`, so the limiter, resolver, and
  helpers all speak the same language.
- **`CachedAPIKey` carries project defaults** — defaults are read at
  auth time and Redis-cached alongside the API key (no extra hot-path
  query). The limiter resolves caps in-memory.
- Unit tests for `LimitSpec`, `CachedAPIKey.ProjectDefaultLimitSpec()`,
  and `ResolveCustomerLimit` precedence
  (`internal/shared/models/customer_limits_test.go`,
  `internal/shared/database/resolver_test.go`).
- `test_guide/cost-control-slice-a.md` — end-to-end local test guide
  covering project defaults, tiers, streaming enforcement, downgrade
  swap, and the μUSD precision floor.

### Changed

- **All USD cap/spend columns widened to `DECIMAL(14,6)` (μUSD
  precision).** `projects.{monthly_cap_usd,current_month_spend_usd,
  default_*_usd}`, `customer_limits.*_usd`, `customer_tiers.*_usd`,
  `gateway_logs.cost_usd`, `customer_spend.total_spend_usd`. Sub-cent
  caps (down to `$0.000001`) now work — previously anything below
  `$0.005` rounded to `$0.00`. Helper functions
  `get_customer_{daily,monthly}_spend` updated to match. Idempotent
  `ALTER COLUMN … TYPE DECIMAL(14,6)` migration block at the bottom of
  `schema/schema.sql` widens existing OSS deployments in place
  (metadata-only on PG 9.2+).
- **All USD values rendered with `%.6f`** in response headers
  (`X-Customer-Spend-Today`, `X-Customer-Limit-Daily`,
  `X-Customer-Remaining-Usd`) and 429 error messages
  (`"daily spend limit exceeded (current: $X.XXXXXX, limit: $Y.YYYYYY)"`).
  Previously a mix of `%.2f` / `%.4f`.
- **Streaming pre-call cost estimate uses real prompt size + `max_tokens`**
  (`internal/gateway/handlers/chat_stream.go`). Was a flat 1000-token
  guess regardless of payload size, which mis-estimated tiny prompts by
  50× and oversized prompts by 100×. The non-streaming path already did
  this; streaming now matches.
- **Renamed Go module and repository** from `github.com/mrmushfiq/llm0-gateway`
  to `github.com/llm0ai/llm0`. Update clone URLs, import paths, and the
  compiled binary name (`llm0`). Old Go module paths no longer resolve.

### Fixed

- **Streaming endpoint enforces per-customer caps.** Before this
  release, `"stream": true` requests bypassed the customer limiter
  entirely — projects with daily caps configured for non-stream were
  still bleeding money on streamed calls from the same end-user.
  `chat_stream.go` now calls `customerLimiter.Check`, applies
  block/downgrade, and writes spend headers before opening the SSE
  stream. (Internal tracking: **P0-1**.)
- **`downgrade` behavior actually swaps the model.** The
  `on_limit_behavior = 'downgrade'` path was wired through the
  resolver/limiter but the handlers never read
  `customerLimitCheck.DowngradeModel` — `block` and `downgrade` were
  effectively the same. Both `chat.go` and `chat_stream.go` now
  `applyCustomerDowngrade(...)` and rebuild the failover chain around
  the cheaper model; the response carries `X-Downgraded: true` and
  `X-Downgraded-Model: <original>`. Tier-level downgrade also works.
  (Internal tracking: **P0-2**.)
- **Schema drift on `label_limits` / `spend_by_label`.** Go code read
  `customer_limits.label_limits` (JSONB, per-label daily caps) and
  wrote `customer_spend.spend_by_label`, but the columns were missing
  from `schema.sql`. Both are now declared in the table DDL and
  re-added by an idempotent `ADD COLUMN IF NOT EXISTS` block.

### Planned

- **Scheduler heartbeat table** to close the v0.1.1 paper cut where
  `SELECT count(*) FROM system_logs` returns zero on a fresh install
  even though the scheduler is healthy. See
  [`design/background-workers.md`](../design/background-workers.md#candidate-fix-for-v012)
  for the proposed `scheduler_heartbeat` design.
- **`manage_limits.sh` auto-invalidates the API-key auth cache** after
  UPDATEs to `projects` (cap, rate limit, cache flags). Today an
  operator has to manually `DEL apikey:*` for changes to propagate
  faster than `CACHE_TTL_SECONDS` (default 1 hour). See
  [`design/enforcement-and-caching.md`](../design/enforcement-and-caching.md)
  → "Propagation delay on config changes".

### Candidates (not committed)

These are loose ideas — promote to **Planned** when confirmed:

- Prometheus `/metrics` endpoint (counters for provider/model/status,
  latency histograms, cache hit rate, failover count, cost total).
- Add `xai-*` (Grok) provider — prefix-based routing is already in
  place.
- Add `deepseek-*` provider via their OpenAI-compatible endpoint.
- `/v1/embeddings` proxy so users can use the bundled embedding service
  through the same auth/rate-limit/spend-cap plumbing.
- Publish pre-built Docker images to GHCR.
- Document streaming integration recipes (LangChain, LlamaIndex, Vercel
  AI SDK) in `docs/integrations/`.
- Switch Redis `maxmemory-policy` from `allkeys-lru` to `noeviction` (or
  a key-prefix-aware alternative) so `spend:*` counters can't be evicted
  under memory pressure.

---

## [0.1.3] — 2026-05-25

Patch release: Ollama streaming cleanup, cost precision fix, and request
validation. No schema changes, no env var changes beyond one new optional
toggle.

### Fixed

- **Filter empty-delta chunks from Ollama streams.** Ollama's
  OpenAI-compatible adapter can emit many `{"delta":{"role":"assistant"}}`
  frames before the first content token. The gateway now drops chunks
  with empty `content` and `tool_calls`, while preserving the first
  `role` chunk and any chunk with `finish_reason`. Enabled by default;
  set `OLLAMA_FILTER_EMPTY_CHUNKS=false` for the raw upstream stream.
  Implementation: `internal/gateway/streaming/ollama_filter.go`, wired
  in `internal/gateway/handlers/chat_stream.go`.

- **Round `cost_usd` to 6 decimals at source.** The `X-Cost-Usd` header
  was formatted with `%.6f` but the JSON body and SSE metadata frame
  wrote the raw `float64`, producing `"cost_usd": 0.0000065999999999999995`
  while the matching header showed `0.000007`. A single
  `math.Round(actualCost*1e6) / 1e6` after each `CalculateCost` call
  aligns all consumers: header, body, metadata frame, and
  `gateway_logs.cost_usd`. Locations: `chat.go` (non-streaming path),
  `chat_stream.go` EOF branch, `chat_stream.go` `postStreamProcessing`.

### Added

- **Request validation** on `/v1/chat/completions` — rejects malformed
  payloads (missing `model`, empty `messages`, invalid roles) with
  OpenAI-style 400 errors before hitting upstream providers.

---

## [0.1.2] — 2026-02-11

Patch release: Redis durability fix + config-propagation doc
corrections. No schema changes, no env var changes, no API changes.

### Fixed

- **Redis AOF persistence actually enabled in `docker-compose.yml`.**
  The README and design doc both stated AOF was on; the compose file
  never set it, and there was no data volume, so a `docker compose
  down` (or an OOM restart) silently wiped every spend counter. The
  redis service now runs with `--appendonly yes --appendfsync everysec`
  and a dedicated `redis_data` named volume. See
  [`design/enforcement-and-caching.md`](../design/enforcement-and-caching.md)
  → "What happens on a Redis failure".
- **Config-propagation docs corrected.** `README.md` and
  `design/enforcement-and-caching.md` previously stated that
  per-project settings (`monthly_cap_usd`, `rate_limit_per_minute`,
  `cache_enabled`, `semantic_cache_enabled`, `semantic_threshold`)
  propagate within `CUSTOMER_LIMIT_CACHE_TTL_SECONDS` (default 60s).
  That is wrong — they ride the Redis `apikey:*` auth cache, which
  uses `CACHE_TTL_SECONDS` (default **3600s / 1 hour**).
  `CUSTOMER_LIMIT_CACHE_TTL_SECONDS` governs only the in-process
  `customer_limits` cache.

### Added (docs only)

- New **"How the cap reaches the Lua script"** section in
  `design/enforcement-and-caching.md` showing the full config path
  from Postgres → Redis auth cache → Go struct → Lua `ARGV[2]`.
  Clarifies that the cap value is never stored in its own Redis key.
- `CUSTOMER_LIMIT_CACHE_TTL_SECONDS` now documented in the env var
  table in `README.md`.
- Updated `CACHE_TTL_SECONDS` description to reflect its dual role
  (exact-match cache TTL **and** API-key auth cache TTL).

### Upgrade notes

```bash
git pull
docker compose down
docker compose up -d
```

The new `redis_data` volume starts empty. That's no worse than any
previous Redis restart — counters rebuild naturally from live traffic.
If you need to reconstruct historical spend, rebuild from
`gateway_logs` (see
[`design/enforcement-and-caching.md`](../design/enforcement-and-caching.md)).

Nothing else needs to be rebuilt: the gateway Go binary and the
embedding image are unchanged.

---

[Unreleased]: https://github.com/llm0ai/llm0/compare/v0.1.3...HEAD
[0.1.3]: https://github.com/llm0ai/llm0/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/llm0ai/llm0/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/llm0ai/llm0/releases/tag/v0.1.1
