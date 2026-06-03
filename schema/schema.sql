-- LLM0 Gateway — Database Schema
-- PostgreSQL 15+  |  Requires pgvector extension for semantic caching
--
-- Run with:
--   psql $DATABASE_URL -f schema/schema.sql
-- Or via Docker Compose:
--   docker compose exec postgres psql -U llm0 -d llm0_gateway -f /schema/schema.sql

-- ============================================================================
-- EXTENSIONS
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS "pgcrypto"; -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS vector;     -- pgvector (semantic caching)

-- ============================================================================
-- PROJECTS
-- Projects are the top-level resource. Each project has its own API keys,
-- cache settings, and spend cap. user_id is a free-form UUID you control —
-- no user table required for the self-hosted gateway.
-- ============================================================================

CREATE TABLE IF NOT EXISTS projects (
    id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,  -- Owner identifier; any UUID you manage
    name    VARCHAR(255) NOT NULL,

    -- Monthly spend cap (gateway blocks requests once exceeded).
    -- DECIMAL(14,6) → μUSD precision (matches gateway_logs.cost_usd) so
    -- caps and accumulated spend can express sub-cent LLM costs without
    -- rounding loss. Max value: $99,999,999.999999.
    monthly_cap_usd         DECIMAL(14,6) DEFAULT 20,
    current_month_spend_usd DECIMAL(14,6) DEFAULT 0,
    spend_reset_at TIMESTAMPTZ DEFAULT date_trunc('month', NOW() + interval '1 month'),

    -- Cache settings (can also be set per-request via headers)
    cache_enabled          BOOLEAN     DEFAULT true,
    semantic_cache_enabled BOOLEAN     DEFAULT false,
    semantic_threshold     DECIMAL(3,2) DEFAULT 0.95, -- cosine similarity threshold
    cache_ttl_seconds      INT         DEFAULT 3600,  -- 1 hour

    -- Per-customer DEFAULT limits (applied to every customer in this project
    -- unless overridden by a tier or a row in customer_limits). All NULL by
    -- default — opt-in. Set via scripts/manage_project_defaults.sh or the
    -- managed-cloud dashboard. See plans/customer-limits-tiers.md.
    -- DECIMAL(14,6) → μUSD precision, see comment on monthly_cap_usd above.
    default_daily_spend_limit_usd   DECIMAL(14,6),
    default_monthly_spend_limit_usd DECIMAL(14,6),
    default_per_request_max_usd     DECIMAL(14,6),
    default_requests_per_minute     INT,
    default_requests_per_hour       INT,
    default_requests_per_day        INT,
    default_on_limit_behavior       VARCHAR(20) DEFAULT 'block',
    default_downgrade_model         VARCHAR(100),

    is_active  BOOLEAN     DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_projects_user   ON projects(user_id);
CREATE INDEX IF NOT EXISTS idx_projects_active ON projects(is_active);

-- ============================================================================
-- API KEYS
-- Format: llm0_live_<32 hex chars>
-- The full key is shown once on creation; only the bcrypt hash is stored.
-- ============================================================================

CREATE TABLE IF NOT EXISTS api_keys (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,

    key_hash   VARCHAR(255) NOT NULL,  -- bcrypt hash of the raw key
    key_prefix VARCHAR(20)  NOT NULL,  -- first 15 chars + "..." shown in UI

    name               VARCHAR(255) NOT NULL,
    rate_limit_per_minute INT DEFAULT 60,

    is_active   BOOLEAN    DEFAULT true,
    last_used_at TIMESTAMPTZ,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX        IF NOT EXISTS idx_api_keys_project ON api_keys(project_id, is_active);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash    ON api_keys(key_hash);

-- ============================================================================
-- GATEWAY LOGS
-- One row per request. Tracks cost, latency, cache status, and failover info.
-- ============================================================================

CREATE TABLE IF NOT EXISTS gateway_logs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID REFERENCES projects(id) ON DELETE CASCADE,
    api_key_id UUID REFERENCES api_keys(id) ON DELETE SET NULL,

    -- Request
    model    VARCHAR(100) NOT NULL,
    provider VARCHAR(50)  NOT NULL,

    -- Tokens & cost
    tokens_in    INT,
    tokens_out   INT,
    tokens_total INT,
    cost_usd     DECIMAL(14,6),

    -- Performance
    latency_ms         INT,
    cache_hit          BOOLEAN DEFAULT false,
    semantic_cache_hit BOOLEAN DEFAULT false,
    similarity_score   REAL,

    -- Routing & failover
    failover_count    INT DEFAULT 0,
    failover_occurred BOOLEAN DEFAULT false,
    final_provider    VARCHAR(50),

    -- Status
    status        VARCHAR(50),  -- 'success', 'error', 'rate_limited', 'cap_exceeded'
    error_message TEXT,

    -- Customer attribution (optional — set via X-Customer-ID header)
    customer_id VARCHAR(255),
    labels      JSONB,  -- Custom labels from X-LLM0-* headers

    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_gateway_logs_project_time ON gateway_logs(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_gateway_logs_cache        ON gateway_logs(project_id, cache_hit);
CREATE INDEX IF NOT EXISTS idx_gateway_logs_cost         ON gateway_logs(project_id, cost_usd);
CREATE INDEX IF NOT EXISTS idx_gateway_logs_customer     ON gateway_logs(customer_id, created_at DESC)
    WHERE customer_id IS NOT NULL;

-- ============================================================================
-- FAILOVER LOGS
-- Detailed record of every provider switch for debugging and analytics.
-- ============================================================================

CREATE TABLE IF NOT EXISTS failover_logs (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID REFERENCES projects(id) ON DELETE CASCADE,
    request_id VARCHAR(255),

    original_model    VARCHAR(100) NOT NULL,
    original_provider VARCHAR(50)  NOT NULL,
    fallback_model    VARCHAR(100) NOT NULL,
    fallback_provider VARCHAR(50)  NOT NULL,

    trigger_reason       VARCHAR(50) NOT NULL,  -- 'rate_limit', 'timeout', 'server_error'
    trigger_status_code  INT,
    trigger_error_message TEXT,

    original_attempt_latency_ms INT,
    fallback_latency_ms         INT,
    total_latency_ms            INT,

    fallback_succeeded     BOOLEAN NOT NULL,
    fallback_error_message TEXT,

    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_failover_logs_project  ON failover_logs(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_failover_logs_trigger  ON failover_logs(trigger_reason, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_failover_logs_provider ON failover_logs(original_provider, fallback_provider);

-- ============================================================================
-- CUSTOMER RATE LIMITING
-- Per-end-user spend caps and request limits within a project.
-- customer_id comes from the X-Customer-ID request header.
-- ============================================================================

CREATE TABLE IF NOT EXISTS customer_limits (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    customer_id VARCHAR(255) NOT NULL,

    -- Spend limits (DECIMAL(14,6) → μUSD precision)
    daily_spend_limit_usd   DECIMAL(14,6),
    monthly_spend_limit_usd DECIMAL(14,6),
    per_request_max_usd     DECIMAL(14,6),

    -- Request limits
    requests_per_minute INT,
    requests_per_hour   INT,
    requests_per_day    INT,

    -- Per-model limits (JSONB): {"gpt-4o": 50, "gpt-4o-mini": null}
    -- null = unlimited, number = max requests per day for that model
    model_limits JSONB,

    -- Per-label daily request limits (JSONB):
    -- {"feature:chat": 1000, "team:support": 500}
    label_limits JSONB,

    -- What to do when a limit is hit: 'block' | 'downgrade' | 'warn'
    on_limit_behavior VARCHAR(20) DEFAULT 'block',
    downgrade_model   VARCHAR(100),  -- used when on_limit_behavior = 'downgrade'

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(project_id, customer_id)
);

CREATE INDEX IF NOT EXISTS idx_customer_limits_project          ON customer_limits(project_id);
CREATE INDEX IF NOT EXISTS idx_customer_limits_project_customer ON customer_limits(project_id, customer_id);

-- Customer spend tracking (actual usage per day)
CREATE TABLE IF NOT EXISTS customer_spend (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    customer_id VARCHAR(255) NOT NULL,

    date DATE NOT NULL,
    hour INT,  -- 0-23 for hourly, NULL for daily aggregate

    total_spend_usd DECIMAL(14,6) DEFAULT 0,
    request_count   INT           DEFAULT 0,

    spend_by_model JSONB DEFAULT '{}'::jsonb,
    spend_by_label JSONB DEFAULT '{}'::jsonb,

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(project_id, customer_id, date, hour)
);

CREATE INDEX IF NOT EXISTS idx_customer_spend_project_customer ON customer_spend(project_id, customer_id, date);
CREATE INDEX IF NOT EXISTS idx_customer_spend_date             ON customer_spend(date);

-- ============================================================================
-- CUSTOMER TIERS
-- Owner-defined "plans" (e.g. 'free', 'pro', 'enterprise' — any slug the
-- owner picks). Customers carry a tier via the X-Customer-Tier request
-- header; the limiter resolves the tier's LimitSpec at request time.
--
-- Precedence on each request:
--   X-Customer-Tier (this table) → project default columns → unlimited
--
-- LLM0 has no built-in tier names. Managed via scripts/manage_tiers.sh
-- (OSS) or the managed-cloud dashboard. See plans/customer-limits-tiers.md.
-- ============================================================================

CREATE TABLE IF NOT EXISTS customer_tiers (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    slug       VARCHAR(64) NOT NULL,    -- owner-defined: 'free', 'pro', '1', etc.

    -- Spend limits (same shape as projects.default_* and customer_limits;
    -- DECIMAL(14,6) → μUSD precision).
    daily_spend_limit_usd   DECIMAL(14,6),
    monthly_spend_limit_usd DECIMAL(14,6),
    per_request_max_usd     DECIMAL(14,6),

    -- Request limits
    requests_per_minute INT,
    requests_per_hour   INT,
    requests_per_day    INT,

    -- Advanced limits (JSONB)
    model_limits JSONB,
    label_limits JSONB,

    -- Behavior on limit: 'block' | 'downgrade' | 'warn'
    on_limit_behavior VARCHAR(20) DEFAULT 'block',
    downgrade_model   VARCHAR(100),

    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(project_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_customer_tiers_project ON customer_tiers(project_id);

-- Helper: total spend for a customer on a given day
CREATE OR REPLACE FUNCTION get_customer_daily_spend(
    p_project_id  UUID,
    p_customer_id VARCHAR(255),
    p_date        DATE DEFAULT CURRENT_DATE
) RETURNS DECIMAL(14,6) LANGUAGE plpgsql AS $$
DECLARE v_total DECIMAL(14,6);
BEGIN
    SELECT COALESCE(SUM(total_spend_usd), 0)
    INTO v_total
    FROM customer_spend
    WHERE project_id  = p_project_id
      AND customer_id = p_customer_id
      AND date        = p_date;
    RETURN v_total;
END; $$;

-- Helper: total spend for a customer in a given month
CREATE OR REPLACE FUNCTION get_customer_monthly_spend(
    p_project_id  UUID,
    p_customer_id VARCHAR(255),
    p_year  INT DEFAULT EXTRACT(YEAR  FROM CURRENT_DATE)::INT,
    p_month INT DEFAULT EXTRACT(MONTH FROM CURRENT_DATE)::INT
) RETURNS DECIMAL(14,6) LANGUAGE plpgsql AS $$
DECLARE v_total DECIMAL(14,6);
BEGIN
    SELECT COALESCE(SUM(total_spend_usd), 0)
    INTO v_total
    FROM customer_spend
    WHERE project_id  = p_project_id
      AND customer_id = p_customer_id
      AND EXTRACT(YEAR  FROM date) = p_year
      AND EXTRACT(MONTH FROM date) = p_month;
    RETURN v_total;
END; $$;

-- ============================================================================
-- EXACT-MATCH CACHE
-- Two-tier: Redis (hot, sub-ms) + Postgres (warm, persistent).
-- Cache key = SHA-256 of (project_id + model + normalized messages).
-- ============================================================================

CREATE TABLE IF NOT EXISTS exact_cache (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID REFERENCES projects(id) ON DELETE CASCADE,

    cache_key VARCHAR(64) UNIQUE NOT NULL,  -- SHA-256 hex

    provider VARCHAR(50)  NOT NULL,
    model    VARCHAR(100) NOT NULL,

    prompt_tokens     INT,
    completion_tokens INT,

    cached_response JSONB NOT NULL,

    hit_count  INT         DEFAULT 0,
    last_hit_at TIMESTAMPTZ DEFAULT NOW(),
    expires_at  TIMESTAMPTZ NOT NULL,

    created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_exact_cache_key          ON exact_cache(cache_key);
CREATE INDEX IF NOT EXISTS idx_exact_cache_project      ON exact_cache(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_exact_cache_expires      ON exact_cache(expires_at);
CREATE INDEX IF NOT EXISTS idx_exact_cache_provider_model ON exact_cache(provider, model);

-- ============================================================================
-- SEMANTIC CACHE
-- Vector similarity search via pgvector.
-- Requires the embedding service to be running (see EMBEDDING_SERVICE_URL).
-- Embeddings: all-MiniLM-L6-v2 (384 dimensions, self-hosted, free).
-- ============================================================================

CREATE TABLE IF NOT EXISTS semantic_cache (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID REFERENCES projects(id) ON DELETE CASCADE,

    cache_key VARCHAR(64) UNIQUE NOT NULL,

    provider VARCHAR(50)  NOT NULL,
    model    VARCHAR(100) NOT NULL,

    embedding       VECTOR(384) NOT NULL,  -- 384-dim all-MiniLM-L6-v2
    original_prompt TEXT        NOT NULL,

    cached_response   JSONB NOT NULL,
    prompt_tokens     INT,
    completion_tokens INT,

    hit_count   INT         DEFAULT 0,
    last_hit_at  TIMESTAMPTZ DEFAULT NOW(),
    expires_at   TIMESTAMPTZ NOT NULL,

    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- HNSW index for fast approximate nearest-neighbour search
CREATE INDEX IF NOT EXISTS idx_semantic_cache_embedding ON semantic_cache
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

CREATE INDEX IF NOT EXISTS idx_semantic_cache_project       ON semantic_cache(project_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_semantic_cache_expires       ON semantic_cache(expires_at);
CREATE INDEX IF NOT EXISTS idx_semantic_cache_provider_model ON semantic_cache(provider, model);

-- ============================================================================
-- MODEL PRICING
-- Used by the cost calculator to estimate request cost before calling the provider.
-- ============================================================================

CREATE TABLE IF NOT EXISTS model_pricing (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider VARCHAR(50)  NOT NULL,
    model    VARCHAR(100) NOT NULL,

    input_per_1k_tokens  DECIMAL(10,8),
    output_per_1k_tokens DECIMAL(10,8),
    context_window       INT,
    supports_streaming   BOOLEAN DEFAULT true,
    supports_functions   BOOLEAN DEFAULT false,

    updated_at TIMESTAMPTZ DEFAULT NOW(),

    UNIQUE(provider, model)
);

CREATE INDEX IF NOT EXISTS idx_model_pricing_lookup ON model_pricing(provider, model);

-- NOTE: Default model pricing is seeded from schema/seed_models.sql.
-- Docker Compose mounts that file into the postgres initdb directory, so it
-- runs automatically on first boot. For non-Docker setups, after applying
-- this file run:  psql $DATABASE_URL -f schema/seed_models.sql
--
-- The seed uses ON CONFLICT DO NOTHING, so it's safe to re-run and will
-- never overwrite user-managed entries (e.g. from scripts/manage_models.sh).

-- ============================================================================
-- SYSTEM LOGS
-- Audit trail for scheduled maintenance jobs (monthly spend reset, log
-- cleanup, cache cleanup, customer-spend reconciliation). Populated by the
-- Scheduler in internal/gateway/workers. Not on the hot path — written once
-- per job run, not per request.
-- ============================================================================

CREATE TABLE IF NOT EXISTS system_logs (
    id          UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type  VARCHAR(64)   NOT NULL,    -- e.g. 'monthly_spend_reset', 'log_cleanup'
    message     TEXT          NOT NULL,
    metadata    JSONB         DEFAULT '{}'::jsonb,
    created_at  TIMESTAMPTZ   DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_system_logs_event_time ON system_logs(event_type, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_system_logs_created    ON system_logs(created_at DESC);

-- ============================================================================
-- UPGRADE PATH FOR EXISTING DATABASES
-- ----------------------------------------------------------------------------
-- `CREATE TABLE IF NOT EXISTS` is skipped wholesale when the table already
-- exists, so columns added to a CREATE TABLE above never reach OSS
-- deployments that were initialized on an older version. These idempotent
-- ALTERs reapply column additions safely. All are nullable / constant-default
-- (PG11+) → metadata-only change, no rewrite, no long locks. Safe on a live
-- DB. Purely additive; renames/drops/type-changes are not done here.
-- ============================================================================

-- projects: per-customer default limits (Slice A, plans/customer-limits-tiers.md)
ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS default_daily_spend_limit_usd   DECIMAL(14,6),
    ADD COLUMN IF NOT EXISTS default_monthly_spend_limit_usd DECIMAL(14,6),
    ADD COLUMN IF NOT EXISTS default_per_request_max_usd     DECIMAL(14,6),
    ADD COLUMN IF NOT EXISTS default_requests_per_minute     INT,
    ADD COLUMN IF NOT EXISTS default_requests_per_hour       INT,
    ADD COLUMN IF NOT EXISTS default_requests_per_day        INT,
    ADD COLUMN IF NOT EXISTS default_on_limit_behavior       VARCHAR(20) DEFAULT 'block',
    ADD COLUMN IF NOT EXISTS default_downgrade_model         VARCHAR(100);

-- customer_limits: label_limits column is read by Go but was missing on
-- older deployments (pre-existing schema drift). Add it idempotently.
ALTER TABLE customer_limits
    ADD COLUMN IF NOT EXISTS label_limits JSONB;

-- customer_spend: spend_by_label column is written by Go's RecordCustomerSpend
-- but was missing on older deployments (pre-existing schema drift).
ALTER TABLE customer_spend
    ADD COLUMN IF NOT EXISTS spend_by_label JSONB DEFAULT '{}'::jsonb;

-- ----------------------------------------------------------------------------
-- USD precision upgrade (μUSD floor)
-- ----------------------------------------------------------------------------
-- Older deployments stored cap/spend columns as DECIMAL(10,2), which rounded
-- any sub-cent value (e.g. $0.000435 of gpt-4o-mini) to $0.00 — limits below
-- $0.01 were unsettable and `current_month_spend_usd` truncated per-request
-- accumulation. Widening to DECIMAL(14,6) gives μUSD precision and matches
-- gateway_logs.cost_usd / customer_spend.total_spend_usd.
--
-- ALTER COLUMN TYPE on a NUMERIC widen is a metadata-only change in Postgres
-- (no table rewrite) as of PG 9.2 when only precision grows and the value
-- range is preserved. Safe on a live DB. Idempotent — running on an already
-- DECIMAL(14,6) column is a no-op rewrite-checked-then-skipped by Postgres.
ALTER TABLE projects
    ALTER COLUMN monthly_cap_usd                TYPE DECIMAL(14,6),
    ALTER COLUMN current_month_spend_usd        TYPE DECIMAL(14,6),
    ALTER COLUMN default_daily_spend_limit_usd  TYPE DECIMAL(14,6),
    ALTER COLUMN default_monthly_spend_limit_usd TYPE DECIMAL(14,6),
    ALTER COLUMN default_per_request_max_usd    TYPE DECIMAL(14,6);

ALTER TABLE customer_limits
    ALTER COLUMN daily_spend_limit_usd   TYPE DECIMAL(14,6),
    ALTER COLUMN monthly_spend_limit_usd TYPE DECIMAL(14,6),
    ALTER COLUMN per_request_max_usd     TYPE DECIMAL(14,6);

ALTER TABLE customer_tiers
    ALTER COLUMN daily_spend_limit_usd   TYPE DECIMAL(14,6),
    ALTER COLUMN monthly_spend_limit_usd TYPE DECIMAL(14,6),
    ALTER COLUMN per_request_max_usd     TYPE DECIMAL(14,6);

-- These columns are already DECIMAL(10,6) on fresh installs; widen the
-- integer side so a single request / day can theoretically exceed $9999.
ALTER TABLE gateway_logs   ALTER COLUMN cost_usd         TYPE DECIMAL(14,6);
ALTER TABLE customer_spend ALTER COLUMN total_spend_usd  TYPE DECIMAL(14,6);

-- ============================================================================
-- AUTO-UPDATE TRIGGER
-- ============================================================================

CREATE OR REPLACE FUNCTION update_updated_at_column()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
    CREATE TRIGGER trg_projects_updated_at        BEFORE UPDATE ON projects        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
EXCEPTION WHEN duplicate_object THEN NULL; END; $$;

DO $$ BEGIN
    CREATE TRIGGER trg_api_keys_updated_at        BEFORE UPDATE ON api_keys        FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
EXCEPTION WHEN duplicate_object THEN NULL; END; $$;

DO $$ BEGIN
    CREATE TRIGGER trg_customer_limits_updated_at BEFORE UPDATE ON customer_limits FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
EXCEPTION WHEN duplicate_object THEN NULL; END; $$;

DO $$ BEGIN
    CREATE TRIGGER trg_customer_tiers_updated_at  BEFORE UPDATE ON customer_tiers  FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
EXCEPTION WHEN duplicate_object THEN NULL; END; $$;

DO $$ BEGIN
    CREATE TRIGGER trg_customer_spend_updated_at  BEFORE UPDATE ON customer_spend  FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();
EXCEPTION WHEN duplicate_object THEN NULL; END; $$;
