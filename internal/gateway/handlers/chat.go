package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"

	"github.com/llm0ai/llm0/internal/gateway/auth"
	"github.com/llm0ai/llm0/internal/gateway/cache"
	"github.com/llm0ai/llm0/internal/gateway/cost"
	"github.com/llm0ai/llm0/internal/gateway/embeddings"
	"github.com/llm0ai/llm0/internal/gateway/failover"
	"github.com/llm0ai/llm0/internal/gateway/providers"
	"github.com/llm0ai/llm0/internal/gateway/ratelimit"
	"github.com/llm0ai/llm0/internal/shared/config"
	"github.com/llm0ai/llm0/internal/shared/database"
	"github.com/llm0ai/llm0/internal/shared/models"
	"github.com/llm0ai/llm0/internal/shared/redis"
)

// ChatHandler handles chat completion requests
type ChatHandler struct {
	db                *database.DB
	redis             *redis.Client
	cfg               *config.Config
	openaiProvider    *providers.OpenAIProvider
	anthropicProvider *providers.AnthropicProvider
	geminiProvider    *providers.GeminiProvider
	ollamaProvider    *providers.OllamaProvider
	costCalculator    *cost.Calculator
	exactCache        *cache.ExactCache
	semanticCache     *cache.SemanticCache
	embeddingClient   *embeddings.Client
	failoverExecutor  *failover.Executor
	failoverLogger    *failover.Logger
	customerLimiter   *ratelimit.Limiter
}

// NewChatHandler creates a new chat handler
func NewChatHandler(db *database.DB, redis *redis.Client, cfg *config.Config) *ChatHandler {
	openaiProvider := providers.NewOpenAIProvider(cfg)
	anthropicProvider := providers.NewAnthropicProvider(cfg)
	geminiProvider := providers.NewGeminiProvider(cfg)
	ollamaProvider := providers.NewOllamaProvider(cfg) // nil when OLLAMA_BASE_URL is not set

	handler := &ChatHandler{
		db:                db,
		redis:             redis,
		cfg:               cfg,
		openaiProvider:    openaiProvider,
		anthropicProvider: anthropicProvider,
		geminiProvider:    geminiProvider,
		ollamaProvider:    ollamaProvider,
		costCalculator:    cost.NewCalculator(db),
		exactCache:        cache.NewExactCache(redis, db),
		failoverExecutor:  failover.NewExecutor(),
		failoverLogger:    failover.NewLogger(db),
		customerLimiter:   ratelimit.NewLimiter(redis, db),
	}

	// Register cloud providers with failover executor
	handler.failoverExecutor.RegisterProvider("openai", func() failover.Provider {
		return openaiProvider
	})
	handler.failoverExecutor.RegisterProvider("anthropic", func() failover.Provider {
		return anthropicProvider
	})
	handler.failoverExecutor.RegisterProvider("google", func() failover.Provider {
		return geminiProvider
	})

	// Register Ollama only when configured
	if ollamaProvider != nil {
		handler.failoverExecutor.RegisterProvider("ollama", func() failover.Provider {
			return ollamaProvider
		})
		fmt.Printf("✅ Ollama provider registered (base_url=%s, mode=%s)\n",
			cfg.OllamaBaseURL, cfg.FailoverMode)
	}

	// Initialize embedding client and semantic cache if configured
	if cfg.EmbeddingServiceURL != "" {
		handler.embeddingClient = embeddings.NewClient(cfg.EmbeddingServiceURL)
		handler.semanticCache = cache.NewSemanticCache(
			db,
			handler.embeddingClient,
			0.95,        // Default similarity threshold
			1*time.Hour, // Default TTL
		)
		fmt.Printf("✅ Semantic cache initialized (URL: %s)\n", cfg.EmbeddingServiceURL)
	} else {
		fmt.Println("⚠️  Semantic cache disabled (no EMBEDDING_SERVICE_URL)")
	}

	providerCount := 3
	if ollamaProvider != nil {
		providerCount = 4
	}
	fmt.Printf("✅ Failover executor initialized with %d providers\n", providerCount)

	return handler
}

// LLMProvider interface for provider abstraction
type LLMProvider interface {
	ChatCompletion(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error)
	ValidateModel(model string) bool
}

// detectProvider detects which provider to use based on model name and failover mode.
func (h *ChatHandler) detectProvider(model string) (string, LLMProvider) {
	// local_only: skip cloud validation entirely — send everything to Ollama.
	if h.cfg.FailoverMode == "local_only" {
		if h.ollamaProvider != nil {
			return "ollama", h.ollamaProvider
		}
		return "", nil
	}

	// Check cloud providers for known models.
	if h.openaiProvider.ValidateModel(model) {
		return "openai", h.openaiProvider
	}
	if h.anthropicProvider.ValidateModel(model) {
		return "anthropic", h.anthropicProvider
	}
	if h.geminiProvider.ValidateModel(model) {
		return "google", h.geminiProvider
	}

	// Unknown model: fall through to Ollama (it accepts any pulled model).
	if h.ollamaProvider != nil {
		return "ollama", h.ollamaProvider
	}

	return "", nil
}

// validateChatRequest rejects bodies that parse as JSON but omit required
// fields. Common culprit: copy-pasted curl bodies with curly/smart quotes
// (U+201C / U+201D) where the "model" and "messages" keys never bind and
// arrive empty — surfacing as a confusing 400 from the upstream provider
// rather than a clear 400 from the gateway.
func validateChatRequest(req providers.ChatRequest) string {
	if strings.TrimSpace(req.Model) == "" {
		return "model is required (check for curly/smart quotes in JSON — use straight \" characters)"
	}
	if len(req.Messages) == 0 {
		return "messages must contain at least one entry (empty messages often means invalid JSON keys from curly/smart quotes in the body)"
	}
	for i, msg := range req.Messages {
		if strings.TrimSpace(msg.Role) == "" {
			return fmt.Sprintf("messages[%d].role is required", i)
		}
	}
	return ""
}

// ChatCompletions handles POST /v1/chat/completions
func (h *ChatHandler) ChatCompletions(c *gin.Context) {
	startTime := time.Now()

	// Get validated API key from auth middleware
	apiKey, ok := auth.GetAPIKey(c)
	if !ok {
		c.JSON(500, gin.H{"error": "internal_error", "message": "API key not found in context"})
		return
	}

	// Parse request
	var req providers.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}
	if msg := validateChatRequest(req); msg != "" {
		c.JSON(400, gin.H{"error": "invalid_request", "message": msg})
		return
	}

	// If streaming is requested, route to streaming handler
	if req.Stream {
		// Store request in context so streaming handler can access it
		c.Set("parsed_request", req)
		h.ChatCompletionsStream(c)
		return
	}

	// Detect provider and validate model
	providerName, provider := h.detectProvider(req.Model)
	if provider == nil {
		c.JSON(400, gin.H{"error": "invalid_model", "message": fmt.Sprintf("Model %s is not supported", req.Model)})
		return
	}

	fmt.Printf("📡 Routing to %s for model %s\n", providerName, req.Model)

	// Get failover chain for this model.
	// Chain composition respects FAILOVER_MODE (cloud_first / local_first / local_only / cloud_only).
	// Failover is disabled for streaming requests (can't retry mid-stream).
	chain := failover.GetFailoverChain(req.Model, h.cfg)
	if chain != nil && !req.Stream {
		fmt.Printf("🔄 Failover enabled: %d providers in chain\n", len(chain.Steps))
	} else if req.Stream {
		fmt.Println("⚡ Streaming mode: failover disabled")
		chain = nil
	}

	ctx := c.Request.Context()

	// Step 1: Check rate limit
	rateLimitKey := fmt.Sprintf("ratelimit:key:%s", apiKey.KeyID)
	allowed, remaining, resetTime, err := h.redis.CheckRateLimit(
		ctx,
		rateLimitKey,
		apiKey.RateLimitPerMinute, // capacity
		apiKey.RateLimitPerMinute, // refill rate (per minute)
		1,                         // requested (1 request)
	)

	if err != nil {
		fmt.Printf("⚠️ Rate limit check failed: %v (fail-open)\n", err)
	} else if !allowed {
		c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", apiKey.RateLimitPerMinute))
		c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
		c.Header("X-RateLimit-Reset", fmt.Sprintf("%d", resetTime))
		c.JSON(429, gin.H{
			"error":       "rate_limit_exceeded",
			"message":     "Too many requests. Please try again later.",
			"retry_after": resetTime,
		})
		return
	}

	// Step 2: Check customer rate limits (if customer_id provided)
	customerID := c.GetHeader("X-Customer-ID")
	// X-Customer-Tier is an owner-defined slug (e.g. 'free', 'pro') the
	// project owner attaches to each request. Treated as server-to-server
	// trust input — never accept from untrusted browser clients. See
	// plans/customer-limits-tiers.md.
	customerTier := c.GetHeader("X-Customer-Tier")
	var customerLabels models.Labels
	var customerLimitCheck *ratelimit.CheckResult

	if customerID != "" {
		// Extract custom labels from headers (X-LLM0-*)
		customerLabels = make(models.Labels)
		for key, values := range c.Request.Header {
			if len(key) > 7 && key[:7] == "X-Llm0-" {
				labelKey := key[7:] // Remove "X-LLM0-" prefix
				if len(values) > 0 {
					customerLabels[labelKey] = values[0]
				}
			}
		}

		// Estimate cost for this request (for pre-check)
		// Uses the detected provider so Ollama requests correctly estimate to $0.
		estimatedCost := h.estimateRequestCost(providerName, req.Model, req.Messages, req.MaxTokens)

		// Check customer limits
		customerLimitCheck, err = h.customerLimiter.Check(ctx, &ratelimit.CheckRequest{
			ProjectID:  apiKey.ProjectID,
			CustomerID: customerID,
			Model:      req.Model,
			CostUSD:    estimatedCost,
			Labels:     customerLabels,
			Tier:       customerTier,
			APIKey:     apiKey,
		})
		if err != nil {
			fmt.Printf("⚠️ Customer rate limit check failed: %v (fail-open)\n", err)
		} else if !customerLimitCheck.Allowed {
			// Add customer limit headers
			for k, v := range customerLimitCheck.Headers {
				c.Header(k, v)
			}

			c.JSON(429, gin.H{
				"error":       "customer_rate_limit_exceeded",
				"message":     customerLimitCheck.Reason,
				"customer_id": customerID,
			})
			return
		}

		// Add customer spend headers (even if allowed)
		if customerLimitCheck != nil {
			if customerLimitCheck.DailySpendLimit != nil {
				c.Header("X-Customer-Spend-Today", fmt.Sprintf("%.6f", customerLimitCheck.DailySpend))
				c.Header("X-Customer-Limit-Daily", fmt.Sprintf("%.6f", *customerLimitCheck.DailySpendLimit))
				remaining := *customerLimitCheck.DailySpendLimit - customerLimitCheck.DailySpend
				c.Header("X-Customer-Remaining-Usd", fmt.Sprintf("%.6f", remaining))
			}

			// Add custom warning headers
			for k, v := range customerLimitCheck.Headers {
				c.Header(k, v)
			}
		}
	}

	// Step 2b: Apply downgrade if the customer hit a spend cap configured with
	// on_limit_behavior = "downgrade". Route to the cheaper model and rebuild
	// the failover chain for it. Everything after this point (cache keys, cost
	// estimate, execution, logging) uses the downgraded model.
	if newProviderName, ok := h.applyCustomerDowngrade(customerLimitCheck, &req); ok {
		providerName = newProviderName
		chain = failover.GetFailoverChain(req.Model, h.cfg)
		c.Header("X-Downgraded", "true")
		c.Header("X-Downgraded-Model", req.Model)
	}

	// Step 3: Check exact match cache (if enabled)
	var cacheHit bool
	var semanticCacheHit bool
	var similarityScore float32
	var cachedResponse *providers.ChatResponse

	if apiKey.CacheEnabled {
		// 2a. Try exact cache first (< 1ms)
		cacheKey, err := h.exactCache.CacheKey(apiKey.ProjectID, providerName, req.Model, req.Messages)
		if err == nil {
			cachedResponse, cacheHit, err = h.exactCache.Get(ctx, cacheKey)
			if err != nil {
				fmt.Printf("⚠️ Exact cache check failed: %v\n", err)
			}

			if cacheHit {
				// Return exact cached response immediately
				// Cache hits cost $0 since we're not calling the LLM API
				cachedResponse.LatencyMs = int(time.Since(startTime).Milliseconds())
				cachedResponse.CostUSD = 0 // Cache hits are free
				c.Header("X-Cache-Hit", "exact")
				c.Header("X-Cost-USD", "0.000000") // Cache hits cost $0
				c.Header("X-Tokens-Prompt", fmt.Sprintf("%d", cachedResponse.Usage.PromptTokens))
				c.Header("X-Tokens-Completion", fmt.Sprintf("%d", cachedResponse.Usage.CompletionTokens))
				c.Header("X-Tokens-Total", fmt.Sprintf("%d", cachedResponse.Usage.TotalTokens))
				c.Header("X-Provider", providerName)
				c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
				c.JSON(200, cachedResponse)

				// Log cache hit in background
				go h.logRequest(context.Background(), apiKey, providerName, req, cachedResponse, true, false, 0, nil, "", customerID, customerLabels)
				return
			}
		}

		// 2b. Try semantic cache if exact miss and semantic is enabled (< 20ms)
		fmt.Printf("🔍 Semantic cache check: enabled=%v, cache=%v\n", apiKey.SemanticCacheEnabled, h.semanticCache != nil)
		if apiKey.SemanticCacheEnabled && h.semanticCache != nil {
			fmt.Println("🔎 Checking semantic cache...")
			cachedResponse, semanticCacheHit, similarityScore, err = h.semanticCache.Get(
				ctx, apiKey.ProjectID, providerName, req.Model, req.Messages)
			if err != nil {
				fmt.Printf("⚠️ Semantic cache check failed: %v\n", err)
			}

			if semanticCacheHit {
				// Return semantically similar cached response
				cachedResponse.LatencyMs = int(time.Since(startTime).Milliseconds())
				c.Header("X-Cache-Hit", "semantic")
				c.Header("X-Cache-Similarity", fmt.Sprintf("%.3f", similarityScore))
				c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
				c.JSON(200, cachedResponse)

				// Log cache hit in background
				go h.logRequest(context.Background(), apiKey, providerName, req, cachedResponse, false, true, similarityScore, nil, "", customerID, customerLabels)
				return
			}
		}
	}

	// Step 3: Check spend cap BEFORE making API call
	spendKey := fmt.Sprintf("spend:project:%s:%s", apiKey.ProjectID, time.Now().Format("2006-01"))

	// Estimate cost based on actual prompt size and client-supplied max_tokens.
	// This is used to pre-check the project's monthly_cap_usd so huge prompts
	// can't silently overshoot the cap.
	promptTokens := estimatePromptTokens(req.Messages)
	estimatedCost, err := h.costCalculator.EstimateCostForRequest(providerName, req.Model, promptTokens, req.MaxTokens)
	if err != nil {
		fmt.Printf("⚠️ Cost estimation failed: %v\n", err)
		if providerName == "ollama" {
			estimatedCost = 0 // Local models are free
		} else {
			estimatedCost = 0.10 // Conservative fallback for unknown cloud models
		}
	}

	// Check if we can afford this request
	canAfford, currentSpend, cap, err := h.redis.CheckSpendCap(ctx, spendKey, estimatedCost, apiKey.MonthlyCap)
	if err != nil {
		fmt.Printf("⚠️ Spend cap check failed: %v (fail-open)\n", err)
	} else if !canAfford {
		c.JSON(402, gin.H{
			"error":         "spend_cap_exceeded",
			"message":       "Monthly spend cap reached",
			"current_spend": currentSpend,
			"monthly_cap":   cap,
		})
		return
	}

	// Step 4: Make request to LLM provider (with failover)
	requestID := uuid.New().String()
	failoverResult := h.failoverExecutor.Execute(ctx, req, chain)

	if !failoverResult.Success {
		c.JSON(500, gin.H{
			"error":   "provider_error",
			"message": failoverResult.Error.Error(),
		})
		// Log error in background
		go h.logRequest(context.Background(), apiKey, providerName, req, nil, false, false, 0, failoverResult, requestID, customerID, customerLabels)
		return
	}

	// Extract response and final provider
	response := failoverResult.Response
	finalProvider := failoverResult.FinalProvider

	// Log failover event if it occurred
	if failoverResult.FailoverOccurred {
		fmt.Printf("✅ Failover succeeded: %s/%s -> %s/%s\n",
			failoverResult.OriginalProvider, failoverResult.OriginalModel,
			failoverResult.FinalProvider, failoverResult.FinalModel)

		// Log failover to database (background)
		go h.failoverLogger.LogFailover(context.Background(), apiKey.ProjectID, requestID, failoverResult)
	}

	// Step 5: Calculate actual cost (use final provider/model after failover)
	actualCost, err := h.costCalculator.CalculateCost(
		finalProvider,
		failoverResult.FinalModel,
		response.Usage.PromptTokens,
		response.Usage.CompletionTokens,
	)
	if err != nil {
		fmt.Printf("⚠️ Cost calculation failed: %v\n", err)
		actualCost = estimatedCost // Use estimate as fallback
	}
	actualCost = math.Round(actualCost*1e6) / 1e6

	// Step 6: Track actual spend (adjust for estimate)
	spendAdjustment := actualCost - estimatedCost
	if spendAdjustment != 0 {
		_, _, _, err = h.redis.CheckSpendCap(ctx, spendKey, spendAdjustment, apiKey.MonthlyCap)
		if err != nil {
			fmt.Printf("⚠️ Spend adjustment failed: %v\n", err)
		}
	}

	// Step 7: Store in caches (if enabled) - use final provider/model after failover
	if apiKey.CacheEnabled {
		// Store in exact cache
		cacheKey, err := h.exactCache.CacheKey(apiKey.ProjectID, finalProvider, failoverResult.FinalModel, req.Messages)
		if err == nil {
			cacheTTL := apiKey.CacheTTL
			if cacheTTL == 0 {
				cacheTTL = h.cfg.CacheTTLSeconds
			}
			if err := h.exactCache.Set(ctx, apiKey.ProjectID, cacheKey, finalProvider, failoverResult.FinalModel, response, cacheTTL); err != nil {
				fmt.Printf("⚠️ Failed to cache in exact cache: %v\n", err)
			}
		}

		// Store in semantic cache (if enabled)
		if apiKey.SemanticCacheEnabled && h.semanticCache != nil {
			fmt.Println("💾 Storing in semantic cache...")
			if err := h.semanticCache.Set(ctx, apiKey.ProjectID, finalProvider, failoverResult.FinalModel, req.Messages, response); err != nil {
				fmt.Printf("⚠️ Failed to cache in semantic cache: %v\n", err)
			} else {
				fmt.Println("✅ Stored in semantic cache")
			}
		}
	}

	// Step 8: Log request in background
	go h.logRequest(context.Background(), apiKey, finalProvider, req, response, false, false, 0, failoverResult, requestID, customerID, customerLabels)

	// Step 8.5: Record customer request (if customer_id provided).
	// Dispatched asynchronously: this performs 3+N Redis round trips plus a
	// DB write for analytics, none of which need to complete before we
	// respond to the client. The brief (~ms) window of stale counters is
	// acceptable — the next request will converge on the correct values.
	if customerID != "" {
		recordReq := &ratelimit.CheckRequest{
			ProjectID:  apiKey.ProjectID,
			CustomerID: customerID,
			Model:      failoverResult.FinalModel,
			CostUSD:    actualCost,
			Labels:     customerLabels,
			Tier:       customerTier,
			APIKey:     apiKey,
		}
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.customerLimiter.RecordRequest(bgCtx, recordReq); err != nil {
				fmt.Printf("⚠️ Failed to record customer request (async): %v\n", err)
			}
		}()
	}

	// Step 9: Return response with cost
	totalLatency := int(time.Since(startTime).Milliseconds())
	response.LatencyMs = totalLatency
	response.CostUSD = actualCost
	response.LatencyMs = totalLatency

	c.Header("X-Cache-Hit", "miss")
	c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
	c.Header("X-Cost-USD", fmt.Sprintf("%.6f", actualCost))
	c.Header("X-Tokens-Prompt", fmt.Sprintf("%d", response.Usage.PromptTokens))
	c.Header("X-Tokens-Completion", fmt.Sprintf("%d", response.Usage.CompletionTokens))
	c.Header("X-Tokens-Total", fmt.Sprintf("%d", response.Usage.TotalTokens))
	c.Header("X-Provider", finalProvider) // Show which provider was used
	if failoverResult.FailoverOccurred {
		c.Header("X-Failover", "true")
		c.Header("X-Original-Provider", failoverResult.OriginalProvider)
	}
	c.JSON(200, response)

	fmt.Printf("✅ Request completed in %dms (provider=%s, cost=$%.6f)\n",
		totalLatency, finalProvider, actualCost)
}

// logRequest logs the request to the database
func (h *ChatHandler) logRequest(
	ctx context.Context,
	apiKey *models.CachedAPIKey,
	provider string,
	req providers.ChatRequest,
	resp *providers.ChatResponse,
	exactCacheHit bool,
	semanticCacheHit bool,
	similarityScore float32,
	failoverResult *failover.FailoverResult,
	requestID string,
	customerID string,
	customerLabels models.Labels,
) {
	// Calculate cost
	var costUSD float64
	var promptTokens, completionTokens, totalTokens int

	if resp != nil {
		promptTokens = resp.Usage.PromptTokens
		completionTokens = resp.Usage.CompletionTokens
		totalTokens = resp.Usage.TotalTokens

		cost, err := h.costCalculator.CalculateCost(provider, req.Model, promptTokens, completionTokens)
		if err == nil {
			costUSD = cost
		}
	}

	// Failover metadata
	failoverOccurred := false
	finalProvider := provider
	failoverCount := 0

	if failoverResult != nil {
		failoverOccurred = failoverResult.FailoverOccurred
		finalProvider = failoverResult.FinalProvider
		failoverCount = failoverResult.AttemptsCount - 1 // Number of failover attempts
	}

	// Insert log (column names match schema)
	query := `
		INSERT INTO gateway_logs (
			id, project_id, api_key_id, provider, model,
			status, tokens_in, tokens_out, tokens_total,
			latency_ms, cost_usd, cache_hit, semantic_cache_hit, similarity_score,
			failover_occurred, final_provider, failover_count,
			customer_id, labels,
			created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, NOW())
	`

	latencyMs := 0
	status := "success"
	if resp != nil {
		latencyMs = resp.LatencyMs
	}
	if failoverResult != nil && !failoverResult.Success {
		status = "error"
	}

	// Determine if any cache was hit
	anyCacheHit := exactCacheHit || semanticCacheHit

	// Convert customer_id (string) to sql.NullString
	var customerIDNull sql.NullString
	if customerID != "" {
		customerIDNull = sql.NullString{String: customerID, Valid: true}
	}

	// Convert labels to JSONB (or NULL)
	var labelsJSON interface{}
	if len(customerLabels) > 0 {
		labelsJSON = customerLabels
	}

	_, err := h.db.ExecContext(ctx, query,
		uuid.New(),
		apiKey.ProjectID,
		apiKey.KeyID,
		provider,
		req.Model,
		status,
		promptTokens,
		completionTokens,
		totalTokens,
		latencyMs,
		costUSD,
		anyCacheHit,
		semanticCacheHit,
		similarityScore,
		failoverOccurred,
		finalProvider,
		failoverCount,
		customerIDNull,
		labelsJSON,
	)

	if err != nil {
		fmt.Printf("⚠️ Failed to log request: %v\n", err)
	}
}

// estimatePromptTokens produces a rough prompt-token count from OpenAI-format
// messages using the industry-standard "4 characters per token" approximation,
// plus a small per-message overhead for role + message boundary tokens.
//
// It's fast (no tokenizer dependencies) and accurate to within ~10% which is
// plenty for pre-flight cost estimation.
func estimatePromptTokens(messages []openai.ChatCompletionMessage) int {
	const (
		charsPerToken      = 4
		overheadPerMessage = 4 // role + formatting overhead
	)

	totalChars := 0
	for _, msg := range messages {
		totalChars += len(msg.Role) + len(msg.Content) + overheadPerMessage
	}

	tokens := totalChars / charsPerToken
	if tokens < 10 {
		tokens = 10 // floor so empty-ish prompts don't round to zero
	}
	return tokens
}

// applyCustomerDowngrade swaps req.Model to the customer's configured cheaper
// model when the limiter signaled a downgrade (on_limit_behavior =
// "downgrade" and a spend cap would otherwise be exceeded). It returns the
// re-detected provider name and whether a downgrade was applied; the caller is
// responsible for rebuilding the failover chain for the new model.
//
// If the configured downgrade_model isn't recognized by any provider, the
// downgrade is skipped (the request proceeds on the original model) so a
// misconfigured downgrade target can't break live traffic.
func (h *ChatHandler) applyCustomerDowngrade(check *ratelimit.CheckResult, req *providers.ChatRequest) (string, bool) {
	if check == nil || !check.ShouldDegrade || check.DowngradeModel == nil {
		return "", false
	}
	newModel := strings.TrimSpace(*check.DowngradeModel)
	if newModel == "" || newModel == req.Model {
		return "", false
	}
	providerName, provider := h.detectProvider(newModel)
	if provider == nil {
		fmt.Printf("⚠️ Downgrade model %q not recognized; keeping %q\n", newModel, req.Model)
		return "", false
	}
	fmt.Printf("⬇️  Customer over limit: downgrading %s → %s\n", req.Model, newModel)
	req.Model = newModel
	return providerName, true
}

// estimateRequestCost estimates the USD cost of a request before making it.
// Used for pre-checking customer daily/monthly spend limits.
//
// Delegates token counting to estimatePromptTokens and cost math to the cost
// package. Returns 0 for Ollama.
func (h *ChatHandler) estimateRequestCost(
	provider, model string,
	messages []openai.ChatCompletionMessage,
	maxTokens *int,
) float64 {
	promptTokens := estimatePromptTokens(messages)

	cost, err := h.costCalculator.EstimateCostForRequest(provider, model, promptTokens, maxTokens)
	if err != nil {
		// Model pricing not found — fall back to a conservative upper bound so
		// customer spend caps still have a sensible ceiling.
		if provider == "ollama" {
			return 0
		}
		return 0.05
	}
	return cost
}
