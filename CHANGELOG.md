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

- **Admin REST API** (`internal/gateway/admin/`) for managing projects and
  API keys over HTTP instead of `psql`: `GET/POST /v1/admin/projects`,
  `GET/PATCH /v1/admin/projects/:id`, `GET/POST
  /v1/admin/projects/:id/api-keys`, `PATCH
  /v1/admin/projects/:id/api-keys/:key_id`. This is the M0 milestone of
  the managed-platform roadmap (`plans/managed/06-milestones-and-roadmap.md`)
  — it's what a future dashboard (or anyone scripting the gateway) talks
  to instead of raw SQL. The existing `scripts/*.sh` helpers are
  unaffected and remain the quickest path for a one-off local change.
- **Plane separation: the admin API runs on its own port.** A second
  `http.Server` (`ADMIN_LISTEN_ADDR`, default `:8081`) is started
  alongside the existing public one (`PORT`, `8080`) in
  `cmd/gateway/main.go` — `/v1/admin/*` does not exist on the public
  port at all. This is the primary defense for an otherwise-sensitive
  API living inside the public OSS repo: in production the admin port is
  bound to an internal-only network (a cloud provider's private
  networking, a Docker-internal network) and never exposed to the
  internet, independent of whether `ADMIN_TOKEN` leaks. See
  `plans/managed/07-deployment-and-ops.md` §1a for the full rationale.
  Leaving `ADMIN_TOKEN` unset (the default) disables the admin listener
  entirely, so self-hosters who don't need it never open the port.
- **Admin REST API: tiers.** `GET/POST /v1/admin/projects/:id/tiers` and
  `DELETE /v1/admin/projects/:id/tiers/:slug`, wrapping the existing
  `customer_tiers` repository (`internal/shared/database/customer_tiers.go`)
  that already backed `scripts/manage_tiers.sh`. `POST` is a full
  replace-or-create (an omitted cap field means "no limit"), matching the
  script's semantics exactly.
- **`scripts/admin_smoke.sh`** — curl-based walkthrough (create project →
  create API key → list projects → list API keys → create tier → list
  tiers → delete tier) proving the admin API end to end, the "done when"
  criterion for the M0 milestone.

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
- Sub-token-cost USD precision (`DECIMAL(16,8)`) — only relevant for
  micro-billing scenarios where a single token of `gpt-4o-mini` input
  (~$0.00000015) needs to be represented exactly. μUSD (6 decimals) is
  enough for every cap and request cost LLM gateways realistically see.

---

## [0.4.0] — 2026-07-19

**Streaming failover + config-driven failover chains.** Failover now works
for `"stream": true` requests too (up to the first byte), and the
cross-provider fallback chains are no longer a hand-maintained per-model
map — they're derived from six config values, so a new model release
means bumping an env var instead of editing Go source.

> **Note:** v0.3.0's changelog flagged the `customer_limits` table drop and
> `CUSTOMER_LIMIT_CACHE_TTL_SECONDS` removal for "v0.4.0." Neither shipped
> in this release — both are deferred to a later one; nothing changes for
> existing deployments on that front.

### Added

- **Pre-first-byte streaming failover** (`failover.Executor.ExecuteStream`,
  `internal/gateway/failover/executor.go`). Each chain step's stream is
  opened *and* its first chunk received before the gateway commits to it —
  SSE headers aren't sent to the client until a provider has actually
  started responding. A 429/5xx/timeout/connection error before the first
  chunk transparently retries the next provider in the chain, invisibly to
  the client. Once a byte has reached the client the stream is final —
  matching the safety line every other gateway (LiteLLM, Portkey,
  OpenRouter) draws, since a mid-stream retry would double-bill the caller
  or splice two providers' output together. `chat_stream.go` rewritten
  around this: SSE headers are committed to only after the winning
  provider yields its first chunk.
- **Config-driven cross-provider failover chains.** Every model is
  classified `flagship` or `cheap` from its name (`mini`/`nano`/`haiku`/
  `flash`/`lite`/`3.5` → cheap, else → flagship); failing over to another
  provider now means "that provider's configured model for the same
  class," resolved at request time in
  `internal/gateway/failover/chains.go`. Replaces the ~15-entry
  `DefaultFailoverChains` map and the separate `ModelTierMap`, both of
  which had already drifted out of sync with `schema/seed_models.sql`
  (`gpt-5.4`, `claude-opus-4-7` had pricing but no failover chain). Any
  never-seen-before `gpt-*`/`claude-*`/`gemini-*` model name now resolves
  a correct chain automatically.
- **Six new env vars** to override the cross-provider defaults per
  deployment without a rebuild: `FAILOVER_OPENAI_FLAGSHIP`,
  `FAILOVER_OPENAI_CHEAP`, `FAILOVER_ANTHROPIC_FLAGSHIP`,
  `FAILOVER_ANTHROPIC_CHEAP`, `FAILOVER_GOOGLE_FLAGSHIP`,
  `FAILOVER_GOOGLE_CHEAP`, plus `FAILOVER_PROVIDER_ORDER` (default
  `openai,anthropic,google`) to control which providers are tried, and in
  what order, after the origin model's own provider.
- **`failover.KnownCloudModels(cfg)`** — the new source for `GET
  /v1/models`'s cloud model list (flagship + cheap per configured
  provider, deduped). Deliberately not exhaustive; any
  `gpt-*`/`claude-*`/`gemini-*` model still works for chat completions.

### Changed

- **`detectProviderForModel`** moved from an `Executor` method to a
  package-level function in `internal/gateway/failover/`, shared by the
  non-streaming executor, the streaming executor, and the new chain
  builder.
- **`failover.LogFailover`** decoupled from `FailoverResult` — takes
  `failoverOccurred bool` and `[]FailoverAttempt` directly so streaming
  and non-streaming paths share one logging function.

### Tested

- Verified live against real OpenAI/Anthropic/Google API calls via Docker
  Compose: `GET /v1/models` reflects `.env` overrides exactly; a
  deliberately-invalid cheap-class model name fails over to the
  configured Anthropic *cheap* model; a deliberately-invalid flagship-
  class model name fails over to the configured Anthropic *flagship*
  model. Both confirmed end-to-end in `failover_logs`.
- 8 new unit tests in `internal/gateway/failover/chains_test.go` covering
  class-based derivation, `FAILOVER_PROVIDER_ORDER` overrides, per-field
  env-style overrides, never-seen-before model names, and
  `KnownCloudModels` dedup. All 13 pre-existing chain tests pass
  unmodified.

### Upgrade notes

No schema changes, no breaking changes. Pure addition — every new
`FAILOVER_*` env var has a code default, so existing deployments behave
identically without touching `.env`.

```bash
git pull
docker compose build gateway
docker compose up -d gateway
```

Optional: copy the new `FAILOVER_*` block from `.env.example` into your
`.env` if you want to pin specific fallback models instead of the code
defaults.

---

## [0.3.0] — 2026-06-XX

**Spend-firewall expansion.** Per-customer enforcement now works without
writing a row per end-user: every project carries default caps, owner-defined
**tiers** (`X-Customer-Tier`) override the defaults, and streaming requests
are finally subject to the same caps as non-streaming. All USD storage
widened to **μUSD precision** (`DECIMAL(14,6)`) so sub-cent caps like
`$0.001/day` are usable. Also folds in the README reframe as "spend
firewall" positioning and the standalone P0-1/P0-2 quick fix that landed
between v0.2.0 and this release.

> **One breaking change worth flagging up top:** per-customer rows in the
> `customer_limits` table are **no longer consulted on the request path**
> — see [Removed](#removed) below for the one-line migration. The
> `monthly_cap_usd` project ceiling and every other v0.2.0 setting keep
> working unchanged.

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
  unlimited. Unknown tier slugs silently fall through to the default
  (typo-resistant). See `internal/shared/database/resolver.go` and the
  new `customer_tiers_cache.go` (~60s in-process TTL). The managed
  Admin API (M1) will add a per-customer override layer above the tier.
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

### Removed

- **Per-customer override path** (`customer_limits` table reads).
  Sub-commands stripped from `scripts/manage_limits.sh`:
  `list-customers`, `set-customer-limit`, `delete-customer-limit`.
  Invoking them now prints a migration message pointing at
  `manage_tiers.sh` / `manage_project_defaults.sh` and exits non-zero.
  The other six sub-commands (`list-keys`, `set-key-rate`, `toggle-key`,
  `list-projects`, `set-project-cap`, `set-project-cache`) are unchanged.
  Migration:
  - For "every end-user should be capped at X" → set it once via
    `./scripts/manage_project_defaults.sh set`.
  - For "this plan caps at X, that plan caps at Y" →
    `./scripts/manage_tiers.sh create` (one tier per plan) and pass
    `X-Customer-Tier: <slug>` per request.
  - For "this one VIP gets a special cap" → assign that customer a
    unique tier slug (e.g. `vip-acme`) with its own caps.
  The `customer_limits` table itself stays in the schema for inspection
  / read-only audits; it is scheduled for full removal in a future
  release (deferred past v0.4.0 — see that release's notes).

### Deprecated

- **`CUSTOMER_LIMIT_CACHE_TTL_SECONDS`** env var. Was the TTL of the
  in-process `customer_limits` cache, which is no longer consulted.
  Reading the value still works (zero behavioral effect) so existing
  Compose / env files don't break. Will be removed in a future release
  (deferred past v0.4.0) alongside the dead Go data-access layer
  (`internal/shared/database/customer_limits_cache.go`,
  `GetCustomerLimit` / `UpsertCustomerLimit` / `DeleteCustomerLimit`).
  Tier and project-default caches are unaffected and have their own
  TTLs (~60 s in-process for tiers; `CACHE_TTL_SECONDS` for the
  API-key/project blob in Redis).

### Upgrade notes

The release introduces new tables, new columns, and one type widen.
Everything is idempotent — re-running `schema.sql` against a v0.1.x
deployment is safe.

```bash
git pull
docker compose build gateway

# Apply additive schema + DECIMAL widen (idempotent, metadata-only)
docker compose exec -T postgres psql -U llm0 -d llm0_gateway \
  -f /docker-entrypoint-initdb.d/01_schema.sql

# Rebuild + restart so the new %.6f formatting and resolver land
docker compose up -d
docker compose exec -T redis redis-cli FLUSHDB
```

Notes:

- **Per-customer override is gone** — see the [Removed](#removed) section
  above. If you had rows in `customer_limits` doing real work in v0.2.0,
  re-express each one as either (a) a project default if it was the
  same cap for everyone, (b) a tier (`manage_tiers.sh create`) if you
  had a small set of plans, or (c) a unique tier slug per VIP. The
  table rows still exist in Postgres — they're just ignored. Project
  defaults / tiers are picked up the moment you flush the API-key cache
  (`docker compose exec -T redis redis-cli FLUSHDB`) or wait out
  `CACHE_TTL_SECONDS`.
- **Project-level `monthly_cap_usd` and API-key rate limits are
  unchanged.** Setting them via `manage_limits.sh set-project-cap` /
  `set-key-rate` works exactly as in v0.2.0.
- **Old caps preserved exactly.** A `monthly_cap_usd = $20.00` row stays
  `$20.00` after the `DECIMAL(14,6)` widen. Only new writes can use sub-cent
  precision.
- **`FLUSHDB` is required** to drop the cached `CachedAPIKey` blobs
  (which carried the old default values). Without it, the gateway keeps
  serving requests with stale project defaults until the Redis TTL
  expires (default 1h).
- **No env vars added** — `CUSTOMER_LIMIT_CACHE_TTL_SECONDS` now also
  governs the in-process `customer_tiers` cache (same ~60s TTL).
- **Header format change** — `X-Customer-Limit-Daily` was 2 decimals
  (`0.01`), now 6 (`0.010000`). Anything parsing these as fixed-width
  decimals needs adjusting.

---

## [0.2.0] — 2026-05-28

Repository transfer + module path rename. No API surface or schema
changes — purely an addressability move ahead of the spend-firewall
reframe.

### Changed

- **Renamed Go module and repository** from
  `github.com/mrmushfiq/llm0-gateway` to `github.com/llm0ai/llm0`.
  Update clone URLs, `go.mod` import paths in dependent projects, and
  the compiled binary name (`llm0`). Old `mrmushfiq/llm0-gateway`
  module paths no longer resolve via `go get`; existing checkouts keep
  working until you run `go mod tidy`.
- **README reframed as a spend firewall** rather than a generic LLM
  gateway. Headline, feature ordering, and the comparison table now
  lead with cost containment (the wedge); routing/cache/failover
  remain documented but as supporting capabilities. No code change.

### Upgrade notes

```bash
# In any dependent Go project
go mod edit -replace github.com/mrmushfiq/llm0-gateway=github.com/llm0ai/llm0
# or, cleaner:
sed -i 's|mrmushfiq/llm0-gateway|llm0ai/llm0|g' go.mod
go mod tidy
```

The Docker Compose / OSS deployment path is unchanged — pull the new
repo and rebuild, no schema migration.

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

[Unreleased]: https://github.com/llm0ai/llm0/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/llm0ai/llm0/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/llm0ai/llm0/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/llm0ai/llm0/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/llm0ai/llm0/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/llm0ai/llm0/releases/tag/v0.1.1
