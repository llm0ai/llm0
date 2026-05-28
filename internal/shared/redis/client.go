package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/llm0ai/llm0/internal/shared/config"
)

// Client wraps Redis client with optimizations from rate_limiter_go
type Client struct {
	client *redis.Client
	cfg    *config.Config

	// Lua scripts for atomic operations
	rateLimitScript       *redis.Script
	spendCheckScript      *redis.Script
	customerSpendScript   *redis.Script
	customerRequestScript *redis.Script

	// Cached script SHAs for performance
	rateLimitScriptSHA       string
	spendCheckScriptSHA      string
	customerSpendScriptSHA   string
	customerRequestScriptSHA string
}

// Rate limit Lua script - atomic rate limiting with token bucket
const rateLimitLuaScript = `
-- Optimized token bucket rate limiter (sub-5ms)
local now = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local refill_rate = tonumber(ARGV[3])
local requested = tonumber(ARGV[4]) or 1

-- Get bucket state in one call
local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'last_refill')
local tokens = tonumber(bucket[1])
local last_refill = tonumber(bucket[2])

-- Initialize or refill tokens
if tokens then
    tokens = math.min(capacity, tokens + ((now - last_refill) * refill_rate * 0.001))
else
    tokens = capacity
end

-- Check and deduct tokens
local allowed = 0
if tokens >= requested then
    tokens = tokens - requested
    allowed = 1
end

-- Update bucket state
redis.call('HMSET', KEYS[1], 'tokens', tokens, 'last_refill', now)
redis.call('EXPIRE', KEYS[1], 900) -- 15 minute TTL

-- Calculate reset time
local reset_time = tokens < capacity and math.ceil((capacity - tokens) / refill_rate) or 0

return {allowed, math.floor(tokens), reset_time}
`

// Spend cap check Lua script - atomic spend tracking and cap enforcement
const spendCheckLuaScript = `
-- Optimized spend cap enforcement (sub-5ms)
local project_key = KEYS[1]
local cost_usd = tonumber(ARGV[1])
local monthly_cap = tonumber(ARGV[2])

-- Get current spend
local current_spend = tonumber(redis.call('GET', project_key)) or 0

-- Check if adding this cost would exceed cap
if current_spend + cost_usd > monthly_cap then
    return {0, current_spend, monthly_cap} -- blocked
end

-- Increment spend
local new_spend = redis.call('INCRBYFLOAT', project_key, cost_usd)

-- Set expiry to end of month (31 days max)
redis.call('EXPIRE', project_key, 2678400)

return {1, tonumber(new_spend), monthly_cap} -- allowed
`

// Customer spend tracking Lua script - atomic spend increment with time windows
const customerSpendLuaScript = `
-- Track customer spend atomically
-- KEYS[1] = spend:customer:{project_id}:{customer_id}:daily:{YYYY-MM-DD}
-- KEYS[2] = spend:customer:{project_id}:{customer_id}:monthly:{YYYY-MM}
-- ARGV[1] = cost_usd
-- ARGV[2] = daily_ttl (in seconds, e.g., 86400 for 24 hours)
-- ARGV[3] = monthly_ttl (in seconds, e.g., 2678400 for 31 days)

local cost_usd = tonumber(ARGV[1])
local daily_ttl = tonumber(ARGV[2])
local monthly_ttl = tonumber(ARGV[3])

-- Increment daily spend
local daily_spend = redis.call('INCRBYFLOAT', KEYS[1], cost_usd)
redis.call('EXPIRE', KEYS[1], daily_ttl)

-- Increment monthly spend
local monthly_spend = redis.call('INCRBYFLOAT', KEYS[2], cost_usd)
redis.call('EXPIRE', KEYS[2], monthly_ttl)

return {tonumber(daily_spend), tonumber(monthly_spend)}
`

// Customer request tracking Lua script - atomic request counting with time windows
const customerRequestLuaScript = `
-- Track customer request counts atomically
-- KEYS[1] = requests:customer:{project_id}:{customer_id}:minute:{timestamp}
-- KEYS[2] = requests:customer:{project_id}:{customer_id}:hour:{timestamp}
-- KEYS[3] = requests:customer:{project_id}:{customer_id}:daily:{date}
-- ARGV[1] = count (usually 1)
-- ARGV[2] = minute_ttl (e.g., 120 for 2 minutes)
-- ARGV[3] = hour_ttl (e.g., 7200 for 2 hours)
-- ARGV[4] = daily_ttl (e.g., 86400 for 24 hours)

local count = tonumber(ARGV[1])
local minute_ttl = tonumber(ARGV[2])
local hour_ttl = tonumber(ARGV[3])
local daily_ttl = tonumber(ARGV[4])

-- Increment all time windows
local minute_count = redis.call('INCRBY', KEYS[1], count)
redis.call('EXPIRE', KEYS[1], minute_ttl)

local hour_count = redis.call('INCRBY', KEYS[2], count)
redis.call('EXPIRE', KEYS[2], hour_ttl)

local daily_count = redis.call('INCRBY', KEYS[3], count)
redis.call('EXPIRE', KEYS[3], daily_ttl)

return {tonumber(minute_count), tonumber(hour_count), tonumber(daily_count)}
`

// NewClient creates an optimized Redis client with connection pooling
func NewClient(ctx context.Context, cfg *config.Config) (*Client, error) {
	// Parse Redis URL
	opt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	// Apply optimized connection pool settings (from rate_limiter_go)
	opt.PoolSize = cfg.RedisPoolSize               // 200 for high concurrency
	opt.MinIdleConns = cfg.RedisMinIdleConns       // 50 to keep connections warm
	opt.MaxRetries = cfg.RedisMaxRetries           // 1 for fail-fast
	opt.MinRetryBackoff = cfg.RedisMinRetryBackoff // 1ms
	opt.MaxRetryBackoff = cfg.RedisMaxRetryBackoff // 5ms
	opt.DialTimeout = cfg.RedisDialTimeout         // 500ms
	opt.ReadTimeout = cfg.RedisReadTimeout         // 100ms
	opt.WriteTimeout = cfg.RedisWriteTimeout       // 100ms
	opt.PoolTimeout = cfg.RedisPoolTimeout         // 500ms

	if cfg.RedisPassword != "" {
		opt.Password = cfg.RedisPassword
	}
	opt.DB = cfg.RedisDB

	client := redis.NewClient(opt)

	// Test connection
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	rc := &Client{
		client:                client,
		cfg:                   cfg,
		rateLimitScript:       redis.NewScript(rateLimitLuaScript),
		spendCheckScript:      redis.NewScript(spendCheckLuaScript),
		customerSpendScript:   redis.NewScript(customerSpendLuaScript),
		customerRequestScript: redis.NewScript(customerRequestLuaScript),
	}

	// Pre-load Lua scripts for optimal performance
	if err := rc.loadScripts(ctx); err != nil {
		return nil, fmt.Errorf("failed to load Lua scripts: %w", err)
	}

	fmt.Printf("✅ Redis client initialized with optimized pool (size=%d, idle=%d)\n",
		cfg.RedisPoolSize, cfg.RedisMinIdleConns)

	return rc, nil
}

// loadScripts pre-loads Lua scripts into Redis for EVALSHA performance
func (rc *Client) loadScripts(ctx context.Context) error {
	// Load rate limit script
	sha, err := rc.rateLimitScript.Load(ctx, rc.client).Result()
	if err != nil {
		return fmt.Errorf("failed to load rate limit script: %w", err)
	}
	rc.rateLimitScriptSHA = sha

	// Verify script is cached
	exists, err := rc.client.ScriptExists(ctx, sha).Result()
	if err != nil || !exists[0] {
		return fmt.Errorf("rate limit script not properly cached")
	}

	// Load spend check script
	sha, err = rc.spendCheckScript.Load(ctx, rc.client).Result()
	if err != nil {
		return fmt.Errorf("failed to load spend check script: %w", err)
	}
	rc.spendCheckScriptSHA = sha

	// Verify script is cached
	exists, err = rc.client.ScriptExists(ctx, sha).Result()
	if err != nil || !exists[0] {
		return fmt.Errorf("spend check script not properly cached")
	}

	// Load customer spend script
	sha, err = rc.customerSpendScript.Load(ctx, rc.client).Result()
	if err != nil {
		return fmt.Errorf("failed to load customer spend script: %w", err)
	}
	rc.customerSpendScriptSHA = sha

	// Load customer request script
	sha, err = rc.customerRequestScript.Load(ctx, rc.client).Result()
	if err != nil {
		return fmt.Errorf("failed to load customer request script: %w", err)
	}
	rc.customerRequestScriptSHA = sha

	fmt.Printf("✅ Lua scripts pre-loaded and cached\n")
	fmt.Printf("   Rate limit SHA: %s\n", rc.rateLimitScriptSHA)
	fmt.Printf("   Spend check SHA: %s\n", rc.spendCheckScriptSHA)
	fmt.Printf("   Customer spend SHA: %s\n", rc.customerSpendScriptSHA)
	fmt.Printf("   Customer request SHA: %s\n", rc.customerRequestScriptSHA)

	return nil
}

// CheckRateLimit performs optimized rate limiting using cached Lua script
func (rc *Client) CheckRateLimit(ctx context.Context, key string, capacity, refillRate, requested int) (allowed bool, remaining, resetTime int, err error) {
	nowMs := time.Now().UnixMilli()

	// Use EVALSHA for maximum performance
	result, err := rc.client.EvalSha(ctx, rc.rateLimitScriptSHA, []string{key}, nowMs, capacity, refillRate, requested).Result()
	if err != nil {
		// Retry with EVAL if script cache miss
		if strings.Contains(err.Error(), "NOSCRIPT") {
			result, err = rc.client.Eval(ctx, rateLimitLuaScript, []string{key}, nowMs, capacity, refillRate, requested).Result()
			if err != nil {
				// Fail open on Redis errors for availability
				return true, capacity, 0, nil
			}
		} else {
			// Fail open on Redis errors
			return true, capacity, 0, nil
		}
	}

	// Parse result
	resultSlice := result.([]interface{})
	return resultSlice[0].(int64) == 1,
		int(resultSlice[1].(int64)),
		int(resultSlice[2].(int64)),
		nil
}

// CheckSpendCap performs optimized spend cap checking using cached Lua script
func (rc *Client) CheckSpendCap(ctx context.Context, projectKey string, costUSD, monthlyCap float64) (allowed bool, currentSpend, cap float64, err error) {
	// Use EVALSHA for maximum performance
	result, err := rc.client.EvalSha(ctx, rc.spendCheckScriptSHA, []string{projectKey}, costUSD, monthlyCap).Result()
	if err != nil {
		// Retry with EVAL if script cache miss
		if strings.Contains(err.Error(), "NOSCRIPT") {
			result, err = rc.client.Eval(ctx, spendCheckLuaScript, []string{projectKey}, costUSD, monthlyCap).Result()
			if err != nil {
				// Fail open on Redis errors
				return true, 0, monthlyCap, nil
			}
		} else {
			// Fail open on Redis errors
			return true, 0, monthlyCap, nil
		}
	}

	// Parse result
	resultSlice := result.([]interface{})
	allowedInt := resultSlice[0].(int64)

	// Handle both int64 (when 0) and string (after INCRBYFLOAT) types
	var currentSpendFloat float64
	switch v := resultSlice[1].(type) {
	case string:
		fmt.Sscanf(v, "%f", &currentSpendFloat)
	case int64:
		currentSpendFloat = float64(v)
	case float64:
		currentSpendFloat = v
	default:
		// Fallback to 0
		currentSpendFloat = 0.0
	}

	// Cap can be int64 or float64
	var capFloat float64
	switch v := resultSlice[2].(type) {
	case int64:
		capFloat = float64(v)
	case float64:
		capFloat = v
	default:
		capFloat = 0.0
	}

	return allowedInt == 1, currentSpendFloat, capFloat, nil
}

// Get retrieves a value from Redis
func (rc *Client) Get(ctx context.Context, key string) (string, error) {
	return rc.client.Get(ctx, key).Result()
}

// Set stores a value in Redis with optional TTL
func (rc *Client) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	return rc.client.Set(ctx, key, value, expiration).Err()
}

// Del deletes keys from Redis
func (rc *Client) Del(ctx context.Context, keys ...string) error {
	return rc.client.Del(ctx, keys...).Err()
}

// Exists checks if a key exists
func (rc *Client) Exists(ctx context.Context, keys ...string) (int64, error) {
	return rc.client.Exists(ctx, keys...).Result()
}

// IncrByFloat increments a float value
func (rc *Client) IncrByFloat(ctx context.Context, key string, value float64) (float64, error) {
	return rc.client.IncrByFloat(ctx, key, value).Result()
}

// HGet gets a hash field value
func (rc *Client) HGet(ctx context.Context, key, field string) (string, error) {
	return rc.client.HGet(ctx, key, field).Result()
}

// HSet sets a hash field value
func (rc *Client) HSet(ctx context.Context, key string, values ...interface{}) error {
	return rc.client.HSet(ctx, key, values...).Err()
}

// HGetAll gets all hash fields
func (rc *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return rc.client.HGetAll(ctx, key).Result()
}

// ============================================================================
// Customer Spend Tracking
// ============================================================================

// TrackCustomerSpend atomically increments customer spend in daily and monthly windows
// Returns: (dailySpend, monthlySpend, error)
func (rc *Client) TrackCustomerSpend(
	ctx context.Context,
	projectID, customerID string,
	costUSD float64,
) (float64, float64, error) {
	now := time.Now()
	dailyKey := fmt.Sprintf("spend:customer:%s:%s:daily:%s",
		projectID, customerID, now.Format("2006-01-02"))
	monthlyKey := fmt.Sprintf("spend:customer:%s:%s:monthly:%s",
		projectID, customerID, now.Format("2006-01"))

	keys := []string{dailyKey, monthlyKey}
	args := []interface{}{
		costUSD,
		86400,   // Daily TTL: 24 hours
		2678400, // Monthly TTL: 31 days
	}

	result, err := rc.customerSpendScript.Run(ctx, rc.client, keys, args...).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("customer spend tracking failed: %w", err)
	}

	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) != 2 {
		return 0, 0, fmt.Errorf("invalid customer spend script result")
	}

	dailySpend := parseFloat(resultSlice[0])
	monthlySpend := parseFloat(resultSlice[1])

	return dailySpend, monthlySpend, nil
}

// GetCustomerSpend retrieves customer spend for a specific time window
func (rc *Client) GetCustomerSpend(
	ctx context.Context,
	projectID, customerID, window string, // window: "daily:YYYY-MM-DD" or "monthly:YYYY-MM"
) (float64, error) {
	key := fmt.Sprintf("spend:customer:%s:%s:%s", projectID, customerID, window)
	val, err := rc.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return 0, nil // No spend yet
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get customer spend: %w", err)
	}

	var spend float64
	_, err = fmt.Sscanf(val, "%f", &spend)
	if err != nil {
		return 0, fmt.Errorf("failed to parse customer spend: %w", err)
	}

	return spend, nil
}

// ============================================================================
// Customer Request Tracking
// ============================================================================

// TrackCustomerRequests atomically increments customer request counts across multiple time windows
// Returns: (minuteCount, hourCount, dailyCount, error)
func (rc *Client) TrackCustomerRequests(
	ctx context.Context,
	projectID, customerID string,
	count int,
) (int, int, int, error) {
	now := time.Now()

	// Generate keys for different time windows
	minuteKey := fmt.Sprintf("requests:customer:%s:%s:minute:%d",
		projectID, customerID, now.Unix()/60)
	hourKey := fmt.Sprintf("requests:customer:%s:%s:hour:%d",
		projectID, customerID, now.Unix()/3600)
	dailyKey := fmt.Sprintf("requests:customer:%s:%s:daily:%s",
		projectID, customerID, now.Format("2006-01-02"))

	keys := []string{minuteKey, hourKey, dailyKey}
	args := []interface{}{
		count,
		120,   // Minute TTL: 2 minutes
		7200,  // Hour TTL: 2 hours
		86400, // Daily TTL: 24 hours
	}

	result, err := rc.customerRequestScript.Run(ctx, rc.client, keys, args...).Result()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("customer request tracking failed: %w", err)
	}

	resultSlice, ok := result.([]interface{})
	if !ok || len(resultSlice) != 3 {
		return 0, 0, 0, fmt.Errorf("invalid customer request script result")
	}

	minuteCount := parseInt(resultSlice[0])
	hourCount := parseInt(resultSlice[1])
	dailyCount := parseInt(resultSlice[2])

	return minuteCount, hourCount, dailyCount, nil
}

// GetCustomerRequestCount retrieves customer request count for a specific time window
func (rc *Client) GetCustomerRequestCount(
	ctx context.Context,
	projectID, customerID, window string,
) (int, error) {
	key := fmt.Sprintf("requests:customer:%s:%s:%s", projectID, customerID, window)
	val, err := rc.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return 0, nil // No requests yet
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get customer request count: %w", err)
	}

	var count int
	_, err = fmt.Sscanf(val, "%d", &count)
	if err != nil {
		return 0, fmt.Errorf("failed to parse customer request count: %w", err)
	}

	return count, nil
}

// ============================================================================
// Batched Customer Counter Fetch (MGET — one round trip)
// ============================================================================

// CustomerCounterSnapshot is a point-in-time view of a customer's spend and
// request counters across standard time windows.
type CustomerCounterSnapshot struct {
	DailySpend     float64
	MonthlySpend   float64
	MinuteRequests int
	HourRequests   int
	DailyRequests  int
}

// GetCustomerCounters fetches all five standard rate-limiting counters in a
// single MGET — one Redis round trip instead of five. This replaces the N
// separate GET calls previously issued from Limiter.Check and is the single
// biggest hot-path win for requests that carry an X-LLM0-Customer-ID header.
//
// Missing keys are returned as zero values (as expected for absent counters).
func (rc *Client) GetCustomerCounters(
	ctx context.Context,
	projectID, customerID string,
) (*CustomerCounterSnapshot, error) {
	now := time.Now()
	dailySpendKey := fmt.Sprintf("spend:customer:%s:%s:daily:%s",
		projectID, customerID, now.Format("2006-01-02"))
	monthlySpendKey := fmt.Sprintf("spend:customer:%s:%s:monthly:%s",
		projectID, customerID, now.Format("2006-01"))
	minuteReqKey := fmt.Sprintf("requests:customer:%s:%s:minute:%d",
		projectID, customerID, now.Unix()/60)
	hourReqKey := fmt.Sprintf("requests:customer:%s:%s:hour:%d",
		projectID, customerID, now.Unix()/3600)
	dailyReqKey := fmt.Sprintf("requests:customer:%s:%s:daily:%s",
		projectID, customerID, now.Format("2006-01-02"))

	vals, err := rc.client.MGet(ctx,
		dailySpendKey, monthlySpendKey,
		minuteReqKey, hourReqKey, dailyReqKey,
	).Result()
	if err != nil {
		return nil, fmt.Errorf("MGET customer counters failed: %w", err)
	}

	snap := &CustomerCounterSnapshot{}
	snap.DailySpend = parseAnyFloat(vals[0])
	snap.MonthlySpend = parseAnyFloat(vals[1])
	snap.MinuteRequests = parseAnyInt(vals[2])
	snap.HourRequests = parseAnyInt(vals[3])
	snap.DailyRequests = parseAnyInt(vals[4])
	return snap, nil
}

// MGetInts fetches multiple counter keys with one round trip and returns a
// slice of ints aligned to the input keys (nil keys → 0).
// Used for per-model / per-label counter lookups.
func (rc *Client) MGetInts(ctx context.Context, keys ...string) ([]int, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	vals, err := rc.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]int, len(vals))
	for i, v := range vals {
		out[i] = parseAnyInt(v)
	}
	return out, nil
}

// IncrWithTTL atomically increments a counter and sets its TTL in one
// round-trip pipeline. Replaces the racy GET → parse → SET pattern previously
// used by Limiter.RecordRequest for per-model and per-label counters.
//
// Both operations are queued in a transaction pipeline so they ship together.
// EXPIRE is issued every call (cheap, idempotent); a small Lua "set TTL only
// on first INCR" would save a few microseconds but adds script-cache overhead.
func (rc *Client) IncrWithTTL(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	pipe := rc.client.TxPipeline()
	incrCmd := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("INCR+EXPIRE pipeline failed: %w", err)
	}
	return incrCmd.Val(), nil
}

// parseAnyFloat handles nil, string, int64, and float64 results from MGET.
func parseAnyFloat(v interface{}) float64 {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case string:
		var f float64
		fmt.Sscanf(t, "%f", &f)
		return f
	case float64:
		return t
	case int64:
		return float64(t)
	}
	return 0
}

// parseAnyInt handles nil, string, int64 results from MGET.
func parseAnyInt(v interface{}) int {
	if v == nil {
		return 0
	}
	switch t := v.(type) {
	case string:
		var i int
		fmt.Sscanf(t, "%d", &i)
		return i
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

// ============================================================================
// Helper functions
// ============================================================================

// parseFloat safely parses a float from interface{}
func parseFloat(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int64:
		return float64(val)
	case string:
		var f float64
		fmt.Sscanf(val, "%f", &f)
		return f
	default:
		return 0.0
	}
}

// parseInt safely parses an int from interface{}
func parseInt(v interface{}) int {
	switch val := v.(type) {
	case int64:
		return int(val)
	case float64:
		return int(val)
	case string:
		var i int
		fmt.Sscanf(val, "%d", &i)
		return i
	default:
		return 0
	}
}

// HealthCheck checks if Redis is healthy
func (rc *Client) HealthCheck(ctx context.Context) error {
	return rc.client.Ping(ctx).Err()
}

// Close closes the Redis connection
func (rc *Client) Close() error {
	return rc.client.Close()
}

// Keys returns all keys matching the pattern (for reconciliation)
func (rc *Client) Keys(ctx context.Context, pattern string) ([]string, error) {
	return rc.client.Keys(ctx, pattern).Result()
}

// HashSecret creates a SHA256 hash (for API key prefixes)
func HashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
