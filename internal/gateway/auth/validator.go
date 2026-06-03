package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/llm0ai/llm0/internal/shared/config"
	"github.com/llm0ai/llm0/internal/shared/database"
	"github.com/llm0ai/llm0/internal/shared/models"
	"github.com/llm0ai/llm0/internal/shared/redis"
)

// Validator handles API key validation with Redis caching
// Implements hot key caching pattern for frequently used keys
type Validator struct {
	db    *database.DB
	redis *redis.Client
	cfg   *config.Config
}

// NewValidator creates a new API key validator
func NewValidator(db *database.DB, redis *redis.Client, cfg *config.Config) *Validator {
	return &Validator{
		db:    db,
		redis: redis,
		cfg:   cfg,
	}
}

// ValidateAPIKey validates an API key with Redis caching
// Returns the cached API key info if valid, or error if invalid
func (v *Validator) ValidateAPIKey(ctx context.Context, apiKey string) (*models.CachedAPIKey, error) {
	// Step 1: Check Redis cache first (hot path, < 1ms)
	cacheKey := v.cacheKey(apiKey)
	cached, err := v.getFromCache(ctx, cacheKey)
	if err == nil && cached != nil {
		// Cache hit! Verify key is still active
		if !cached.IsActive || !cached.ProjectActive {
			return nil, fmt.Errorf("API key is inactive")
		}
		return cached, nil
	}

	// Step 2: Cache miss - lookup in database and verify with bcrypt
	cachedKey, err := v.validateFromDatabase(ctx, apiKey)
	if err != nil {
		return nil, err
	}

	// Step 3: Store in Redis for future requests
	// Use longer TTL for hot keys (frequently accessed)
	ttl := time.Duration(v.cfg.CacheTTLSeconds) * time.Second
	if err := v.storeInCache(ctx, cacheKey, cachedKey, ttl); err != nil {
		// Log error but don't fail the request
		fmt.Printf("⚠️ Failed to cache API key: %v\n", err)
	}

	return cachedKey, nil
}

// validateFromDatabase looks up and verifies the API key in PostgreSQL
func (v *Validator) validateFromDatabase(ctx context.Context, apiKey string) (*models.CachedAPIKey, error) {
	// Query for API key with project info (single query for performance)
	// We co-select project default_* columns so ResolveCustomerLimit() can
	// fall back to a project default without an extra query per request.
	// All default_* columns are nullable (Slice A, plans/customer-limits-tiers.md).
	query := `
		SELECT
			ak.id, ak.project_id, ak.key_hash, ak.rate_limit_per_minute, ak.is_active,
			p.is_active as project_active, p.monthly_cap_usd,
			p.cache_enabled, p.semantic_cache_enabled, p.semantic_threshold, p.cache_ttl_seconds,
			p.default_daily_spend_limit_usd, p.default_monthly_spend_limit_usd,
			p.default_per_request_max_usd,
			p.default_requests_per_minute, p.default_requests_per_hour, p.default_requests_per_day,
			p.default_on_limit_behavior, p.default_downgrade_model
		FROM api_keys ak
		INNER JOIN projects p ON ak.project_id = p.id
		WHERE ak.key_prefix = $1
		LIMIT 1
	`

	// Extract prefix (first 15 chars: "llm0_live_abc...")
	if len(apiKey) < 15 {
		return nil, fmt.Errorf("invalid API key format")
	}
	prefix := apiKey[:15] + "..."

	var (
		keyID                uuid.UUID
		projectID            uuid.UUID
		keyHash              string
		rateLimitPerMinute   int
		isActive             bool
		projectActive        bool
		monthlyCap           float64
		cacheEnabled         bool
		semanticCacheEnabled bool
		semanticThreshold    float64
		cacheTTL             int

		// Nullable project default-limit columns (Slice A).
		defaultDaily      sql.NullFloat64
		defaultMonthly    sql.NullFloat64
		defaultPerReq     sql.NullFloat64
		defaultReqMin     sql.NullInt64
		defaultReqHour    sql.NullInt64
		defaultReqDay     sql.NullInt64
		defaultBehavior   sql.NullString
		defaultDowngrade  sql.NullString
	)

	err := v.db.QueryRowContext(ctx, query, prefix).Scan(
		&keyID, &projectID, &keyHash, &rateLimitPerMinute, &isActive,
		&projectActive, &monthlyCap, &cacheEnabled, &semanticCacheEnabled,
		&semanticThreshold, &cacheTTL,
		&defaultDaily, &defaultMonthly, &defaultPerReq,
		&defaultReqMin, &defaultReqHour, &defaultReqDay,
		&defaultBehavior, &defaultDowngrade,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("API key not found")
	}
	if err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}

	// Verify bcrypt hash
	// Important: We hash the API key with SHA256 first, then verify with bcrypt
	// This is because bcrypt has a 72-byte limit and our keys are 69 chars
	keyHashForBcrypt := redis.HashSecret(apiKey) // SHA256 hash
	if err := bcrypt.CompareHashAndPassword([]byte(keyHash), []byte(keyHashForBcrypt)); err != nil {
		return nil, fmt.Errorf("invalid API key")
	}

	// Check if key and project are active
	if !isActive {
		return nil, fmt.Errorf("API key is inactive")
	}
	if !projectActive {
		return nil, fmt.Errorf("project is inactive")
	}

	cached := &models.CachedAPIKey{
		KeyID:                keyID,
		ProjectID:            projectID,
		RateLimitPerMinute:   rateLimitPerMinute,
		IsActive:             isActive,
		ProjectActive:        projectActive,
		MonthlyCap:           monthlyCap,
		CacheEnabled:         cacheEnabled,
		SemanticCacheEnabled: semanticCacheEnabled,
		SemanticThreshold:    semanticThreshold,
		CacheTTL:             cacheTTL,
		CachedAt:             time.Now(),
	}
	if defaultDaily.Valid {
		v := defaultDaily.Float64
		cached.DefaultDailySpendLimitUSD = &v
	}
	if defaultMonthly.Valid {
		v := defaultMonthly.Float64
		cached.DefaultMonthlySpendLimitUSD = &v
	}
	if defaultPerReq.Valid {
		v := defaultPerReq.Float64
		cached.DefaultPerRequestMaxUSD = &v
	}
	if defaultReqMin.Valid {
		v := int(defaultReqMin.Int64)
		cached.DefaultRequestsPerMinute = &v
	}
	if defaultReqHour.Valid {
		v := int(defaultReqHour.Int64)
		cached.DefaultRequestsPerHour = &v
	}
	if defaultReqDay.Valid {
		v := int(defaultReqDay.Int64)
		cached.DefaultRequestsPerDay = &v
	}
	if defaultBehavior.Valid {
		v := defaultBehavior.String
		cached.DefaultOnLimitBehavior = &v
	}
	if defaultDowngrade.Valid {
		v := defaultDowngrade.String
		cached.DefaultDowngradeModel = &v
	}
	return cached, nil
}

// getFromCache retrieves cached API key from Redis
func (v *Validator) getFromCache(ctx context.Context, cacheKey string) (*models.CachedAPIKey, error) {
	data, err := v.redis.Get(ctx, cacheKey)
	if err != nil {
		return nil, err
	}

	var cached models.CachedAPIKey
	if err := json.Unmarshal([]byte(data), &cached); err != nil {
		return nil, err
	}

	return &cached, nil
}

// storeInCache stores validated API key in Redis
func (v *Validator) storeInCache(ctx context.Context, cacheKey string, cachedKey *models.CachedAPIKey, ttl time.Duration) error {
	data, err := json.Marshal(cachedKey)
	if err != nil {
		return err
	}

	return v.redis.Set(ctx, cacheKey, string(data), ttl)
}

// cacheKey generates Redis cache key for API key
func (v *Validator) cacheKey(apiKey string) string {
	// Use hash to avoid storing full key in Redis key name
	hash := redis.HashSecret(apiKey)
	return fmt.Sprintf("apikey:%s", hash[:16])
}

// InvalidateCache invalidates cached API key (called when key is revoked/updated)
func (v *Validator) InvalidateCache(ctx context.Context, apiKey string) error {
	cacheKey := v.cacheKey(apiKey)
	return v.redis.Del(ctx, cacheKey)
}

// UpdateLastUsed updates the last_used_at timestamp for an API key
func (v *Validator) UpdateLastUsed(ctx context.Context, keyID uuid.UUID) error {
	query := `UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`
	_, err := v.db.ExecContext(ctx, query, keyID)
	return err
}
