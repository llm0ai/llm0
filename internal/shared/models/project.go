package models

import (
	"time"

	"github.com/google/uuid"
)

// Project represents an LLM gateway project with spend caps and caching settings.
type Project struct {
	ID                   uuid.UUID `json:"id" db:"id"`
	UserID               uuid.UUID `json:"user_id" db:"user_id"`
	Name                 string    `json:"name" db:"name"`
	MonthlyCap           float64   `json:"monthly_cap_usd" db:"monthly_cap_usd"`
	CurrentMonthSpend    float64   `json:"current_month_spend_usd" db:"current_month_spend_usd"`
	SpendResetAt         time.Time `json:"spend_reset_at" db:"spend_reset_at"`
	CacheEnabled         bool      `json:"cache_enabled" db:"cache_enabled"`
	SemanticCacheEnabled bool      `json:"semantic_cache_enabled" db:"semantic_cache_enabled"`
	SemanticThreshold    float64   `json:"semantic_threshold" db:"semantic_threshold"`
	CacheTTL             int       `json:"cache_ttl_seconds" db:"cache_ttl_seconds"`
	IsActive             bool      `json:"is_active" db:"is_active"`
	CreatedAt            time.Time `json:"created_at" db:"created_at"`
	UpdatedAt            time.Time `json:"updated_at" db:"updated_at"`
}

// APIKey represents a gateway API key (format: llm0_live_<hex>).
type APIKey struct {
	ID                 uuid.UUID  `json:"id" db:"id"`
	ProjectID          uuid.UUID  `json:"project_id" db:"project_id"`
	KeyHash            string     `json:"-" db:"key_hash"`            // bcrypt hash, never expose
	KeyPrefix          string     `json:"key_prefix" db:"key_prefix"` // first 15 chars + "..."
	Name               string     `json:"name" db:"name"`
	RateLimitPerMinute int        `json:"rate_limit_per_minute" db:"rate_limit_per_minute"`
	IsActive           bool       `json:"is_active" db:"is_active"`
	LastUsedAt         *time.Time `json:"last_used_at" db:"last_used_at"`
	CreatedAt          time.Time  `json:"created_at" db:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at" db:"updated_at"`
}

// CachedAPIKey is the Redis-cached representation of an API key + its project settings.
// Stored after the first DB lookup to avoid repeated bcrypt + DB hits on hot paths.
type CachedAPIKey struct {
	KeyID              uuid.UUID `json:"key_id"`
	ProjectID          uuid.UUID `json:"project_id"`
	RateLimitPerMinute int       `json:"rate_limit_per_minute"`
	IsActive           bool      `json:"is_active"`

	// Denormalized project fields (avoids a second DB query per request)
	ProjectActive        bool    `json:"project_active"`
	MonthlyCap           float64 `json:"monthly_cap"`
	CacheEnabled         bool    `json:"cache_enabled"`
	SemanticCacheEnabled bool    `json:"semantic_cache_enabled"`
	SemanticThreshold    float64 `json:"semantic_threshold"`
	CacheTTL             int     `json:"cache_ttl"`

	// Per-customer DEFAULT limits inherited from the project row. All
	// nullable — present only when the project owner has configured a default
	// via scripts/manage_project_defaults.sh or the managed dashboard. Cached
	// here so ResolveCustomerLimit() costs ZERO extra queries on the hot path.
	DefaultDailySpendLimitUSD   *float64 `json:"default_daily_spend_limit_usd,omitempty"`
	DefaultMonthlySpendLimitUSD *float64 `json:"default_monthly_spend_limit_usd,omitempty"`
	DefaultPerRequestMaxUSD     *float64 `json:"default_per_request_max_usd,omitempty"`
	DefaultRequestsPerMinute    *int     `json:"default_requests_per_minute,omitempty"`
	DefaultRequestsPerHour      *int     `json:"default_requests_per_hour,omitempty"`
	DefaultRequestsPerDay       *int     `json:"default_requests_per_day,omitempty"`
	DefaultOnLimitBehavior      *string  `json:"default_on_limit_behavior,omitempty"`
	DefaultDowngradeModel       *string  `json:"default_downgrade_model,omitempty"`

	CachedAt time.Time `json:"cached_at"`
}

// ProjectDefaultLimitSpec builds a LimitSpec from the cached project default
// columns. Returns nil if no default is configured (all cap fields NULL).
// Centralized here so callers don't reach into individual default_* fields.
func (k *CachedAPIKey) ProjectDefaultLimitSpec() *LimitSpec {
	if k == nil {
		return nil
	}
	spec := &LimitSpec{
		DailySpendLimitUSD:   k.DefaultDailySpendLimitUSD,
		MonthlySpendLimitUSD: k.DefaultMonthlySpendLimitUSD,
		PerRequestMaxUSD:     k.DefaultPerRequestMaxUSD,
		RequestsPerMinute:    k.DefaultRequestsPerMinute,
		RequestsPerHour:      k.DefaultRequestsPerHour,
		RequestsPerDay:       k.DefaultRequestsPerDay,
		DowngradeModel:       k.DefaultDowngradeModel,
	}
	if k.DefaultOnLimitBehavior != nil {
		spec.OnLimitBehavior = LimitBehavior(*k.DefaultOnLimitBehavior)
	}
	if spec.IsEmpty() {
		return nil
	}
	return spec
}

// GatewayLog represents a single logged LLM request for analytics.
type GatewayLog struct {
	ID               uuid.UUID `json:"id" db:"id"`
	ProjectID        uuid.UUID `json:"project_id" db:"project_id"`
	APIKeyID         uuid.UUID `json:"api_key_id" db:"api_key_id"`
	Provider         string    `json:"provider" db:"provider"`
	Model            string    `json:"model" db:"model"`
	StatusCode       int       `json:"status_code" db:"status_code"`
	PromptTokens     int       `json:"prompt_tokens" db:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens" db:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens" db:"total_tokens"`
	LatencyMs        int       `json:"latency_ms" db:"latency_ms"`
	CostUSD          float64   `json:"cost_usd" db:"cost_usd"`
	CacheHit         bool      `json:"cache_hit" db:"cache_hit"`
	SemanticCacheHit bool      `json:"semantic_cache_hit" db:"semantic_cache_hit"`
	UserIP           *string   `json:"user_ip" db:"user_ip"`
	UserAgent        *string   `json:"user_agent" db:"user_agent"`
	ErrorMessage     *string   `json:"error_message" db:"error_message"`
	CreatedAt        time.Time `json:"created_at" db:"created_at"`
}

// ExactCache represents a cached LLM response keyed by SHA-256 of the request.
type ExactCache struct {
	ID               uuid.UUID `json:"id" db:"id"`
	ProjectID        uuid.UUID `json:"project_id" db:"project_id"`
	CacheKey         string    `json:"cache_key" db:"cache_key"`
	Provider         string    `json:"provider" db:"provider"`
	Model            string    `json:"model" db:"model"`
	PromptTokens     int       `json:"prompt_tokens" db:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens" db:"completion_tokens"`
	CachedResponse   string    `json:"cached_response" db:"cached_response"`
	HitCount         int       `json:"hit_count" db:"hit_count"`
	LastHitAt        time.Time `json:"last_hit_at" db:"last_hit_at"`
	ExpiresAt        time.Time `json:"expires_at" db:"expires_at"`
	CreatedAt        time.Time `json:"created_at" db:"created_at"`
}
