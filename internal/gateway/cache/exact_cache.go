package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/gateway/providers"
	"github.com/llm0ai/llm0/internal/shared/database"
	"github.com/llm0ai/llm0/internal/shared/redis"
)

// ExactCache handles exact-match caching of LLM responses
type ExactCache struct {
	redis *redis.Client
	db    *database.DB
}

// NewExactCache creates a new exact cache handler
func NewExactCache(redis *redis.Client, db *database.DB) *ExactCache {
	return &ExactCache{
		redis: redis,
		db:    db,
	}
}

// CacheKey generates a cache key for a request
func (c *ExactCache) CacheKey(projectID uuid.UUID, provider, model string, messages interface{}) (string, error) {
	// Create a stable representation of the request
	requestData := map[string]interface{}{
		"project_id": projectID.String(),
		"provider":   provider,
		"model":      model,
		"messages":   messages,
	}

	// Marshal to JSON for hashing
	jsonData, err := json.Marshal(requestData)
	if err != nil {
		return "", err
	}

	// Generate SHA256 hash
	hash := sha256.Sum256(jsonData)
	return hex.EncodeToString(hash[:]), nil
}

// Get retrieves a cached response if it exists
func (c *ExactCache) Get(ctx context.Context, cacheKey string) (*providers.ChatResponse, bool, error) {
	// Check Redis first (fast path)
	redisKey := fmt.Sprintf("cache:exact:%s", cacheKey)
	data, err := c.redis.Get(ctx, redisKey)
	if err == nil {
		// Cache hit in Redis!
		var response providers.ChatResponse
		if err := json.Unmarshal([]byte(data), &response); err == nil {
			fmt.Printf("✅ Cache HIT (Redis): %s\n", cacheKey[:16])
			return &response, true, nil
		}
	}

	// Check database (warm cache)
	dbResp, found, err := c.getFromDB(ctx, cacheKey)
	if err != nil {
		return nil, false, err
	}
	if found {
		// Store in Redis for next time
		if err := c.storeInRedis(ctx, redisKey, dbResp, 1*time.Hour); err != nil {
			fmt.Printf("⚠️ Failed to cache in Redis: %v\n", err)
		}
		fmt.Printf("✅ Cache HIT (DB -> Redis): %s\n", cacheKey[:16])
		return dbResp, true, nil
	}

	// Cache miss
	fmt.Printf("❌ Cache MISS: %s\n", cacheKey[:16])
	return nil, false, nil
}

// Set stores a response in the cache.
//
// Hot-path design:
//   - Redis write is synchronous (sub-ms; needed so a concurrent request
//     moments later can hit the hot cache).
//   - Postgres write is dispatched to a goroutine with a fresh context so it
//     survives request cancellation. The warm (DB) tier is a durability
//     backstop for Redis eviction/restart; it does NOT need to be visible
//     before we return the response to the caller.
//
// If the gateway crashes before the goroutine completes, we lose at most one
// cache entry from the DB — the Redis copy persists for its TTL and the next
// hit will reconstruct the DB row via Set() again.
func (c *ExactCache) Set(ctx context.Context, projectID uuid.UUID, cacheKey, provider, model string, response *providers.ChatResponse, ttlSeconds int) error {
	redisKey := fmt.Sprintf("cache:exact:%s", cacheKey)
	ttl := time.Duration(ttlSeconds) * time.Second
	if err := c.storeInRedis(ctx, redisKey, response, ttl); err != nil {
		fmt.Printf("⚠️ Failed to cache in Redis: %v\n", err)
	}

	// Dispatch warm-tier persistence asynchronously so the client response
	// isn't blocked on a Postgres INSERT of the full response JSON.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.storeInDB(bgCtx, projectID, cacheKey, provider, model, response, ttlSeconds); err != nil {
			fmt.Printf("⚠️ Failed to cache in DB (async): %v\n", err)
		}
	}()

	fmt.Printf("✅ Cached response: %s (ttl=%ds)\n", cacheKey[:16], ttlSeconds)
	return nil
}

// storeInRedis stores response in Redis
func (c *ExactCache) storeInRedis(ctx context.Context, key string, response *providers.ChatResponse, ttl time.Duration) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return c.redis.Set(ctx, key, string(data), ttl)
}

// getFromDB retrieves cached response from database
func (c *ExactCache) getFromDB(ctx context.Context, cacheKey string) (*providers.ChatResponse, bool, error) {
	query := `
		SELECT cached_response, expires_at
		FROM exact_cache
		WHERE cache_key = $1 AND expires_at > NOW()
		LIMIT 1
	`

	var (
		cachedResponseJSON string
		expiresAt          time.Time
	)

	err := c.db.QueryRowContext(ctx, query, cacheKey).Scan(&cachedResponseJSON, &expiresAt)
	if err != nil {
		// No cached response found
		return nil, false, nil
	}

	// Parse cached response
	var response providers.ChatResponse
	if err := json.Unmarshal([]byte(cachedResponseJSON), &response); err != nil {
		return nil, false, fmt.Errorf("failed to unmarshal cached response: %w", err)
	}

	// Update hit count in background
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c.incrementHitCount(bgCtx, cacheKey)
	}()

	return &response, true, nil
}

// storeInDB stores response in database
func (c *ExactCache) storeInDB(ctx context.Context, projectID uuid.UUID, cacheKey, provider, model string, response *providers.ChatResponse, ttlSeconds int) error {
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return err
	}

	expiresAt := time.Now().Add(time.Duration(ttlSeconds) * time.Second)

	query := `
		INSERT INTO exact_cache (
			id, project_id, cache_key, provider, model,
			prompt_tokens, completion_tokens, cached_response,
			hit_count, last_hit_at, expires_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (cache_key) DO UPDATE SET
			last_hit_at = EXCLUDED.last_hit_at,
			hit_count = exact_cache.hit_count + 1,
			expires_at = EXCLUDED.expires_at
	`

	_, err = c.db.ExecContext(ctx, query,
		uuid.New(),
		projectID,
		cacheKey,
		provider,
		model,
		response.Usage.PromptTokens,
		response.Usage.CompletionTokens,
		string(responseJSON),
		0, // initial hit_count
		time.Now(),
		expiresAt,
		time.Now(),
	)

	return err
}

// incrementHitCount increments the cache hit counter
func (c *ExactCache) incrementHitCount(ctx context.Context, cacheKey string) error {
	query := `
		UPDATE exact_cache
		SET hit_count = hit_count + 1, last_hit_at = NOW()
		WHERE cache_key = $1
	`
	_, err := c.db.ExecContext(ctx, query, cacheKey)
	return err
}

// InvalidateByProject invalidates all cache entries for a project
func (c *ExactCache) InvalidateByProject(ctx context.Context, projectID uuid.UUID) error {
	// Delete from database
	query := `DELETE FROM exact_cache WHERE project_id = $1`
	_, err := c.db.ExecContext(ctx, query, projectID)
	return err
}
