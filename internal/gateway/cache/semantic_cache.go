package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/gateway/embeddings"
	"github.com/llm0ai/llm0/internal/gateway/providers"
	"github.com/llm0ai/llm0/internal/shared/database"
	"github.com/pgvector/pgvector-go"
)

// SemanticCache handles semantic similarity-based caching of LLM responses
type SemanticCache struct {
	db                  *database.DB
	embeddingClient     *embeddings.Client
	similarityThreshold float32       // Default: 0.95 (95% similar)
	ttl                 time.Duration // Default: 1 hour
}

// NewSemanticCache creates a new semantic cache handler
func NewSemanticCache(db *database.DB, embeddingClient *embeddings.Client, similarityThreshold float32, ttl time.Duration) *SemanticCache {
	if similarityThreshold <= 0 || similarityThreshold > 1 {
		similarityThreshold = 0.95 // Default
	}
	if ttl == 0 {
		ttl = 1 * time.Hour // Default
	}

	return &SemanticCache{
		db:                  db,
		embeddingClient:     embeddingClient,
		similarityThreshold: similarityThreshold,
		ttl:                 ttl,
	}
}

// normalizePrompt extracts the user's prompt from messages
// Focuses on the last user message for semantic matching
func (sc *SemanticCache) normalizePrompt(messages interface{}) string {
	// Convert to JSON and back to extract content (handles any message type)
	jsonBytes, err := json.Marshal(messages)
	if err != nil {
		fmt.Printf("⚠️  normalizePrompt: failed to marshal messages: %v\n", err)
		return ""
	}

	// Parse as generic slice
	var msgs []map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &msgs); err != nil {
		fmt.Printf("⚠️  normalizePrompt: failed to unmarshal messages: %v\n", err)
		return ""
	}

	// Extract last user message
	for i := len(msgs) - 1; i >= 0; i-- {
		if role, ok := msgs[i]["role"].(string); ok && role == "user" {
			if content, ok := msgs[i]["content"].(string); ok {
				fmt.Printf("📝 Extracted prompt: '%s'\n", content)
				return content
			}
		}
	}

	fmt.Println("⚠️  normalizePrompt: no user message found")
	return ""
}

// Get searches for semantically similar cached responses
func (sc *SemanticCache) Get(ctx context.Context, projectID uuid.UUID, provider, model string, messages interface{}) (*providers.ChatResponse, bool, float32, error) {
	// 1. Normalize prompt
	prompt := sc.normalizePrompt(messages)
	if prompt == "" {
		return nil, false, 0, nil
	}

	// 2. Generate embedding
	fmt.Printf("🔮 Generating embedding for prompt (length: %d)...\n", len(prompt))
	embedding, err := sc.embeddingClient.GenerateEmbedding(ctx, prompt)
	if err != nil {
		fmt.Printf("❌ Embedding generation failed: %v\n", err)
		return nil, false, 0, fmt.Errorf("generate embedding: %w", err)
	}
	fmt.Printf("✅ Generated embedding (dimensions: %d)\n", len(embedding))

	// 3. Search for similar cached responses
	query := `
		SELECT 
			cached_response,
			1 - (embedding <=> $1) AS similarity,
			id
		FROM semantic_cache
		WHERE project_id = $2
		  AND provider = $3
		  AND model = $4
		  AND expires_at > NOW()
		  AND 1 - (embedding <=> $1) > $5
		ORDER BY embedding <=> $1
		LIMIT 1
	`

	var cachedResponse json.RawMessage
	var similarity float32
	var id uuid.UUID

	pgvec := pgvector.NewVector(embedding)
	err = sc.db.QueryRowContext(ctx, query,
		pgvec, projectID, provider, model, sc.similarityThreshold).Scan(
		&cachedResponse, &similarity, &id)

	if err != nil {
		if err == sql.ErrNoRows {
			// No matching cache found (this is normal)
			fmt.Printf("🔍 Semantic cache: no match found (threshold: %.2f)\n", sc.similarityThreshold)
			return nil, false, 0, nil
		}
		// Actual error - log it
		fmt.Printf("❌ Semantic cache GET error: %v\n", err)
		return nil, false, 0, fmt.Errorf("semantic cache query: %w", err)
	}

	// 4. Parse cached response
	var cachedResp providers.ChatResponse
	if err := json.Unmarshal(cachedResponse, &cachedResp); err != nil {
		return nil, false, 0, fmt.Errorf("unmarshal cached response: %w", err)
	}

	// 5. Update hit statistics (async)
	go sc.incrementHitCount(context.Background(), id)

	fmt.Printf("✅ Semantic cache HIT (similarity: %.3f): %s\n", similarity, id)
	return &cachedResp, true, similarity, nil
}

// Set stores a new response in the semantic cache
func (sc *SemanticCache) Set(ctx context.Context, projectID uuid.UUID, provider, model string, messages interface{}, response *providers.ChatResponse) error {
	// 1. Normalize prompt
	prompt := sc.normalizePrompt(messages)
	if prompt == "" {
		return nil
	}

	// 2. Generate embedding
	embedding, err := sc.embeddingClient.GenerateEmbedding(ctx, prompt)
	if err != nil {
		return fmt.Errorf("generate embedding: %w", err)
	}

	// 3. Generate cache key
	hash := sha256.Sum256([]byte(prompt))
	cacheKey := hex.EncodeToString(hash[:])

	// 4. Marshal response to JSON
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}

	// 5. Insert into database
	query := `
		INSERT INTO semantic_cache (
			project_id, cache_key, provider, model,
			embedding, original_prompt, cached_response,
			prompt_tokens, completion_tokens,
			expires_at, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW()
		)
		ON CONFLICT (cache_key) DO UPDATE SET
			hit_count = semantic_cache.hit_count + 1,
			last_hit_at = NOW()
	`

	pgvec := pgvector.NewVector(embedding)
	expiresAt := time.Now().Add(sc.ttl)

	_, err = sc.db.ExecContext(ctx, query,
		projectID, cacheKey, provider, model,
		pgvec, prompt, responseJSON,
		response.Usage.PromptTokens, response.Usage.CompletionTokens,
		expiresAt)

	if err != nil {
		return fmt.Errorf("insert into semantic_cache: %w", err)
	}

	fmt.Printf("✅ Stored in semantic cache: %s (expires: %s)\n", cacheKey[:16], expiresAt.Format(time.RFC3339))
	return nil
}

// incrementHitCount updates hit statistics (called asynchronously)
func (sc *SemanticCache) incrementHitCount(ctx context.Context, id uuid.UUID) {
	query := `
		UPDATE semantic_cache
		SET hit_count = hit_count + 1,
			last_hit_at = NOW()
		WHERE id = $1
	`
	_, err := sc.db.ExecContext(ctx, query, id)
	if err != nil {
		fmt.Printf("⚠️ Failed to increment hit count: %v\n", err)
	}
}

// GetStats returns cache statistics for a project
func (sc *SemanticCache) GetStats(ctx context.Context, projectID uuid.UUID) (map[string]interface{}, error) {
	query := `
		SELECT 
			COUNT(*) as total_entries,
			COALESCE(SUM(hit_count), 0) as total_hits,
			COALESCE(AVG(hit_count), 0) as avg_hits,
			COALESCE(MAX(hit_count), 0) as max_hits
		FROM semantic_cache
		WHERE project_id = $1
		  AND expires_at > NOW()
	`

	var totalEntries int
	var totalHits int
	var avgHits float64
	var maxHits int

	err := sc.db.QueryRowContext(ctx, query, projectID).Scan(
		&totalEntries, &totalHits, &avgHits, &maxHits)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"total_entries": totalEntries,
		"total_hits":    totalHits,
		"avg_hits":      avgHits,
		"max_hits":      maxHits,
	}, nil
}

// CleanupExpired removes expired cache entries
// Should be called periodically (e.g., daily via cron/lambda)
func (sc *SemanticCache) CleanupExpired(ctx context.Context) (int64, error) {
	query := `
		WITH deleted AS (
			DELETE FROM semantic_cache
			WHERE expires_at < NOW()
			RETURNING id
		)
		SELECT COUNT(*) FROM deleted
	`

	var deletedCount int64
	err := sc.db.QueryRowContext(ctx, query).Scan(&deletedCount)
	if err != nil {
		return 0, fmt.Errorf("cleanup expired semantic cache: %w", err)
	}

	if deletedCount > 0 {
		fmt.Printf("🧹 Cleaned semantic_cache: deleted %d expired entries\n", deletedCount)
	}

	return deletedCount, nil
}
