package handlers

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/llm0ai/llm0/internal/gateway/auth"
	"github.com/llm0ai/llm0/internal/gateway/failover"
	"github.com/llm0ai/llm0/internal/gateway/providers"
	"github.com/llm0ai/llm0/internal/gateway/ratelimit"
	"github.com/llm0ai/llm0/internal/gateway/streaming"
	"github.com/llm0ai/llm0/internal/shared/models"
)

// openProviderStream opens a live stream for one (provider, model) attempt.
// Used directly for the non-failover case and as the failover.StreamOpener
// callback for GetFailoverChain-driven attempts — see ExecuteStream in
// internal/gateway/failover/executor.go for how errors returned here are
// turned into pre-first-byte failover.
func (h *ChatHandler) openProviderStream(ctx context.Context, providerName string, req providers.ChatRequest) (failover.StreamReader, error) {
	switch providerName {
	case "openai":
		return h.openaiProvider.ChatCompletionStream(ctx, req)
	case "anthropic":
		return h.anthropicProvider.ChatCompletionStream(ctx, req)
	case "google":
		return h.geminiProvider.ChatCompletionStream(ctx, req)
	case "ollama":
		if h.ollamaProvider == nil {
			return nil, fmt.Errorf("ollama not configured (set OLLAMA_BASE_URL)")
		}
		return h.ollamaProvider.ChatCompletionStream(ctx, req)
	default:
		return nil, fmt.Errorf("streaming not supported for provider %s", providerName)
	}
}

// ChatCompletionsStream handles streaming chat completion requests.
//
// Failover — pre-first-byte only. This used to be disabled entirely for
// streaming ("can't retry mid-stream"), which is true, but incomplete: a
// step can fail before it ever sends a byte (bad key, unknown model,
// connection error), and that failure is completely invisible to the
// client if we haven't committed to SSE yet. Every other gateway agrees on
// this line (LiteLLM's FallbackStreamWrapper, Portkey, OpenRouter): retry
// freely before the first byte, never after. See failover.Executor.ExecuteStream
// for the mechanics — same retry/skip/stop matrix as the non-streaming path.
//
//   - Steps 1-6 below (rate limit, customer limits + downgrade, cache,
//     project cap) run before any provider call, exactly like the
//     non-streaming path — a rejection here is still a plain JSON response.
//   - Step 7 primes every step in the failover chain (opens the stream AND
//     waits for its first chunk) before sending any SSE header. A total
//     failure at this point is a plain JSON error response (400/500), not
//     an SSE frame — nothing has been written to the client yet.
//   - Step 8 commits: SSE headers go out, the already-consumed first chunk
//     is forwarded, and failover is over for this request. Every failure
//     past this point is an SSE error frame, never a retry.
func (h *ChatHandler) ChatCompletionsStream(c *gin.Context) {
	startTime := time.Now()

	// Disable the http.Server WriteTimeout for this request only.
	// SSE connections can be open for minutes (long reasoning outputs, Ollama
	// on CPU, agent tool-calling chains); the server-wide 60s WriteTimeout
	// would otherwise truncate the stream. Other routes keep the standard
	// timeout. Fails quietly on Go <1.20 or if the writer isn't the stdlib
	// ResponseWriter — streaming still works, just with the outer timeout.
	if rc := http.NewResponseController(c.Writer); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}

	// Get validated API key from auth middleware
	apiKey, ok := auth.GetAPIKey(c)
	if !ok {
		c.JSON(500, gin.H{"error": "internal_error", "message": "API key not found in context"})
		return
	}

	// Get pre-parsed request from context (set by main handler)
	reqInterface, exists := c.Get("parsed_request")
	if !exists {
		c.JSON(500, gin.H{"error": "internal_error", "message": "Request not found in context"})
		return
	}

	req, ok := reqInterface.(providers.ChatRequest)
	if !ok {
		c.JSON(500, gin.H{"error": "internal_error", "message": "Invalid request type"})
		return
	}

	// Validate that streaming is enabled
	if !req.Stream {
		c.JSON(400, gin.H{"error": "invalid_request", "message": "Stream must be true"})
		return
	}

	// Detect provider and validate model
	providerName, provider := h.detectProvider(req.Model)
	if provider == nil {
		c.JSON(400, gin.H{"error": "invalid_model", "message": fmt.Sprintf("Model %s is not supported", req.Model)})
		return
	}

	fmt.Printf("⚡ Streaming request: %s via %s\n", req.Model, providerName)

	ctx := c.Request.Context()

	// Extract customer ID, tier, and labels for tracking.
	// X-Customer-Tier is an owner-defined slug (e.g. 'free', 'pro') the
	// project owner attaches to each request. Treated as server-to-server
	// trust input — never accept from untrusted browser clients. See
	// plans/customer-limits-tiers.md.
	customerID := c.GetHeader("X-Customer-ID")
	customerTier := c.GetHeader("X-Customer-Tier")
	var customerLabels models.Labels
	if customerID != "" {
		customerLabels = make(models.Labels)
		for key, values := range c.Request.Header {
			if len(key) > 7 && key[:7] == "X-Llm0-" {
				labelKey := key[7:]
				if len(values) > 0 {
					customerLabels[labelKey] = values[0]
				}
			}
		}
	}

	// Step 1: Check rate limit
	rateLimitKey := fmt.Sprintf("ratelimit:key:%s", apiKey.KeyID)
	allowed, remaining, resetTime, err := h.redis.CheckRateLimit(
		ctx,
		rateLimitKey,
		apiKey.RateLimitPerMinute,
		apiKey.RateLimitPerMinute,
		1,
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

	// Step 2: Check per-customer limits (if X-Customer-ID provided).
	// Mirrors the non-streaming path so streaming requests can't bypass
	// per-customer spend caps, request limits, and model/label limits.
	if customerID != "" {
		estimatedCustomerCost := h.estimateRequestCost(providerName, req.Model, req.Messages, req.MaxTokens)

		customerLimitCheck, cErr := h.customerLimiter.Check(ctx, &ratelimit.CheckRequest{
			ProjectID:  apiKey.ProjectID,
			CustomerID: customerID,
			Model:      req.Model,
			CostUSD:    estimatedCustomerCost,
			Labels:     customerLabels,
			Tier:       customerTier,
			APIKey:     apiKey,
		})
		if cErr != nil {
			fmt.Printf("⚠️ Customer rate limit check failed: %v (fail-open)\n", cErr)
		} else if !customerLimitCheck.Allowed {
			for k, v := range customerLimitCheck.Headers {
				c.Header(k, v)
			}
			c.JSON(429, gin.H{
				"error":       "customer_rate_limit_exceeded",
				"message":     customerLimitCheck.Reason,
				"customer_id": customerID,
			})
			return
		} else if customerLimitCheck != nil {
			// Surface spend headers + warnings even when allowed.
			if customerLimitCheck.DailySpendLimit != nil {
				c.Header("X-Customer-Spend-Today", fmt.Sprintf("%.6f", customerLimitCheck.DailySpend))
				c.Header("X-Customer-Limit-Daily", fmt.Sprintf("%.6f", *customerLimitCheck.DailySpendLimit))
				remainingUSD := *customerLimitCheck.DailySpendLimit - customerLimitCheck.DailySpend
				c.Header("X-Customer-Remaining-Usd", fmt.Sprintf("%.6f", remainingUSD))
			}
			for k, v := range customerLimitCheck.Headers {
				c.Header(k, v)
			}

			// Apply downgrade if the customer hit a spend cap configured with
			// on_limit_behavior = "downgrade": route streaming to the cheaper
			// model. The failover chain built in step 7 is rebuilt off the
			// (possibly downgraded) req.Model, so this stays correct.
			if newProviderName, ok := h.applyCustomerDowngrade(customerLimitCheck, &req); ok {
				providerName = newProviderName
				c.Header("X-Downgraded", "true")
				c.Header("X-Downgraded-Model", req.Model)
			}
		}
	}

	// Step 3: Check cache (if cache hit, return full response - no streaming)
	if apiKey.CacheEnabled {
		cacheKey, err := h.exactCache.CacheKey(apiKey.ProjectID, providerName, req.Model, req.Messages)
		if err == nil {
			cachedResponse, hit, err := h.exactCache.Get(ctx, cacheKey)
			if err != nil {
				fmt.Printf("⚠️ Cache check failed: %v\n", err)
			}
			if hit {
				fmt.Println("✅ Cache HIT - returning full response (not streaming)")
				// Return cached response immediately (not streaming)
				// Cache hits cost $0 since we're not calling the LLM API
				cachedResponse.LatencyMs = int(time.Since(startTime).Milliseconds())
				cachedResponse.CostUSD = 0 // Cache hits are free
				c.Header("X-Cache-Hit", "exact")
				c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
				c.Header("X-Cost-USD", "0.000000") // Cache hits cost $0
				c.Header("X-Tokens-Prompt", fmt.Sprintf("%d", cachedResponse.Usage.PromptTokens))
				c.Header("X-Tokens-Completion", fmt.Sprintf("%d", cachedResponse.Usage.CompletionTokens))
				c.Header("X-Tokens-Total", fmt.Sprintf("%d", cachedResponse.Usage.TotalTokens))
				c.Header("X-Provider", providerName)
				c.JSON(200, cachedResponse)

				go h.logRequest(context.Background(), apiKey, providerName, req, cachedResponse, true, false, 0, false, 0, "", customerID, customerLabels)
				return
			}
		}
	}

	// Step 4: Check project spend cap BEFORE streaming.
	// Estimate cost from the REAL prompt size + client-supplied max_tokens
	// (same helper the non-stream path uses) so the streaming project-cap
	// check matches what the request will actually bill. Previously this
	// path used a flat 1000-token guess which under-charged short prompts
	// and over-charged long ones — see plans/cost-control-audit.md P0-3.
	spendKey := fmt.Sprintf("spend:project:%s:%s", apiKey.ProjectID, time.Now().Format("2006-01"))
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

	// Step 5: Get failover chain for this model.
	// Chain composition respects FAILOVER_MODE (cloud_first / local_first /
	// local_only / cloud_only) — identical to the non-streaming path.
	chain := failover.GetFailoverChain(req.Model, h.cfg)
	if chain != nil {
		fmt.Printf("🔄 Streaming failover enabled (pre-first-byte only): %d providers in chain\n", len(chain.Steps))
	}

	// Step 6: Prime the chain — open each step's stream and wait for its
	// first chunk before sending any SSE header. See failover.ExecuteStream.
	requestID := uuid.New().String()
	opener := func(attemptCtx context.Context, step failover.FailoverStep, attemptReq providers.ChatRequest) (failover.StreamReader, error) {
		attemptReq.Model = step.Model
		return h.openProviderStream(attemptCtx, step.Provider, attemptReq)
	}
	streamResult := h.failoverExecutor.ExecuteStream(ctx, req, chain, opener)

	if !streamResult.Success {
		// Nothing has been written to the client yet — a plain JSON error,
		// same status codes the non-streaming path would use.
		fmt.Printf("❌ Streaming failed before first byte (all chain steps failed): %v\n", streamResult.Error)
		c.JSON(500, gin.H{
			"error":   "provider_error",
			"message": streamResult.Error.Error(),
		})
		go h.logRequest(context.Background(), apiKey, streamResult.FinalProvider, req, nil, false, false, 0, streamResult.FailoverOccurred, streamResult.AttemptsCount-1, requestID, customerID, customerLabels)
		return
	}

	finalProviderName := streamResult.FinalProvider
	finalModel := streamResult.FinalModel
	stream := streamResult.Stream
	defer stream.Close()

	if streamResult.FailoverOccurred {
		fmt.Printf("✅ Streaming failover succeeded: %s/%s -> %s/%s\n",
			streamResult.OriginalProvider, streamResult.OriginalModel, finalProviderName, finalModel)
		go h.failoverLogger.LogFailover(context.Background(), apiKey.ProjectID, requestID, streamResult.FailoverOccurred, streamResult.Attempts)
	}

	// Step 7: Commit to SSE. Every response from this point on is a
	// `data: ` frame, including errors — failover is over; see file header.
	streaming.SetSSEHeaders(c)
	c.Header("X-Cache-Hit", "miss")
	c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))
	c.Header("X-Provider", finalProviderName)
	if streamResult.FailoverOccurred {
		c.Header("X-Failover", "true")
		c.Header("X-Original-Provider", streamResult.OriginalProvider)
	}

	// Step 8: Stream chunks to client and collect for caching
	collector := streaming.NewStreamCollector(finalProviderName, finalModel)
	failoverCount := streamResult.AttemptsCount - 1
	if failoverCount < 0 {
		failoverCount = 0
	}

	// Estimate prompt tokens for cost calculation
	// We'll update with actual count if provided in the stream
	promptText := ""
	for _, msg := range req.Messages {
		promptText += msg.Content + " "
	}
	estimatedPromptTokens := len(promptText) / 4 // Rough estimate: 4 chars per token
	collector.PromptTokens = estimatedPromptTokens

	fmt.Println("🔄 Streaming started...")

	// Active only for Ollama when the user hasn't opted into raw chunks.
	// nil = forward every chunk unchanged.
	var ollamaFilter *streaming.OllamaStreamFilter
	if finalProviderName == "ollama" && h.cfg.OllamaFilterEmptyChunks {
		ollamaFilter = streaming.NewOllamaStreamFilter()
	}

	// The first chunk was already consumed during pre-first-byte priming
	// (step 6) — forward (if not filtered) + absorb it before pulling the
	// rest from the stream.
	firstChunk := streamResult.FirstChunk
	if ollamaFilter.Forward(firstChunk) {
		if err := streaming.SendSSEData(c, firstChunk); err != nil {
			fmt.Printf("⚠️ Client disconnected: %v\n", err)
			return
		}
	}
	if len(firstChunk.Choices) > 0 {
		delta := firstChunk.Choices[0].Delta
		collector.AddChunk(delta.Content)
		if firstChunk.Choices[0].FinishReason != "" {
			collector.SetFinishReason(string(firstChunk.Choices[0].FinishReason))
		}
	}
	if firstChunk.Usage != nil {
		collector.AddUsage(*firstChunk.Usage)
	}

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			// Stream completed successfully
			fmt.Println("✅ Streaming completed")

			// Calculate and send cost info before [DONE]
			collector.EstimateTokensIfNeeded()
			fullResponse := collector.ToResponse()

			actualCost, err := h.costCalculator.CalculateCost(finalProviderName, finalModel, fullResponse.Usage.PromptTokens, fullResponse.Usage.CompletionTokens)
			if err != nil {
				fmt.Printf("⚠️ Cost calculation failed: %v\n", err)
				actualCost = estimatedCost
			}
			actualCost = math.Round(actualCost*1e6) / 1e6

			// Send cost metadata before [DONE]
			costData := map[string]interface{}{
				"object": "chat.completion.chunk.metadata",
				"usage": map[string]interface{}{
					"prompt_tokens":     fullResponse.Usage.PromptTokens,
					"completion_tokens": fullResponse.Usage.CompletionTokens,
					"total_tokens":      fullResponse.Usage.TotalTokens,
				},
				"cost_usd":   actualCost,
				"latency_ms": int(time.Since(startTime).Milliseconds()),
				"provider":   finalProviderName,
			}
			if err := streaming.SendSSEData(c, costData); err == nil {
				c.Writer.Flush()
			}

			streaming.SendSSEDone(c)
			break
		}

		if err != nil {
			// Error during streaming — strictly mid-stream (after the first
			// chunk already went out): no retry, no failover, matches every
			// other gateway.
			fmt.Printf("❌ Streaming error: %v\n", err)
			streaming.SendSSEError(c, err)
			go h.logRequest(context.Background(), apiKey, finalProviderName, req, nil, false, false, 0, streamResult.FailoverOccurred, failoverCount, requestID, customerID, customerLabels)
			return
		}

		// Send chunk to client only if the filter (when active) allows it.
		// Internal bookkeeping below runs for every chunk regardless, so we
		// never lose finish_reason / usage / content data even when the
		// client stream is being de-noised.
		if ollamaFilter.Forward(chunk) {
			if err := streaming.SendSSEData(c, chunk); err != nil {
				// Client disconnected
				fmt.Printf("⚠️ Client disconnected: %v\n", err)
				return
			}
		}

		// Collect chunk for post-processing (caching, cost, logs)
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta
			collector.AddChunk(delta.Content)

			if chunk.Choices[0].FinishReason != "" {
				collector.SetFinishReason(string(chunk.Choices[0].FinishReason))
			}
		}

		// Capture usage info (sent in last chunk)
		if chunk.Usage != nil {
			collector.AddUsage(*chunk.Usage)
		}
	}

	// Step 9: Post-stream processing
	go h.postStreamProcessing(context.Background(), apiKey, finalProviderName, req, finalModel, collector, requestID, estimatedCost, spendKey, startTime, customerID, customerTier, customerLabels, streamResult.FailoverOccurred, failoverCount)

	fmt.Printf("✅ Streaming request completed in %dms\n", int(time.Since(startTime).Milliseconds()))
}

// postStreamProcessing handles caching and logging after stream completes
func (h *ChatHandler) postStreamProcessing(
	ctx context.Context,
	apiKey *models.CachedAPIKey,
	providerName string,
	req providers.ChatRequest,
	finalModel string,
	collector *streaming.StreamCollector,
	requestID string,
	estimatedCost float64,
	spendKey string,
	startTime time.Time,
	customerID string,
	customerTier string,
	customerLabels models.Labels,
	failoverOccurred bool,
	failoverCount int,
) {
	// Convert collected data to full response
	fullResponse := collector.ToResponse()
	fullResponse.ID = requestID

	// Calculate actual cost — final provider/model, which may differ from
	// req.Model after a pre-first-byte failover.
	actualCost, err := h.costCalculator.CalculateCost(
		providerName,
		finalModel,
		collector.PromptTokens,
		collector.CompletionTokens,
	)
	if err != nil {
		fmt.Printf("⚠️ Cost calculation failed: %v\n", err)
		actualCost = estimatedCost
	}
	actualCost = math.Round(actualCost*1e6) / 1e6

	fullResponse.CostUSD = actualCost

	// Adjust spend (difference between estimated and actual)
	spendAdjustment := actualCost - estimatedCost
	if spendAdjustment != 0 {
		_, _, _, err = h.redis.CheckSpendCap(ctx, spendKey, spendAdjustment, apiKey.MonthlyCap)
		if err != nil {
			fmt.Printf("⚠️ Spend adjustment failed: %v\n", err)
		}
	}

	// Record per-customer spend + counters (if X-Customer-ID provided) so
	// streaming requests count toward per-customer daily/monthly caps and
	// rate limits.
	if customerID != "" {
		if err := h.customerLimiter.RecordRequest(ctx, &ratelimit.CheckRequest{
			ProjectID:  apiKey.ProjectID,
			CustomerID: customerID,
			Model:      finalModel,
			CostUSD:    actualCost,
			Labels:     customerLabels,
			Tier:       customerTier,
			APIKey:     apiKey,
		}); err != nil {
			fmt.Printf("⚠️ Failed to record customer request (stream): %v\n", err)
		}
	}

	// Cache the full response (if caching enabled) — keyed by the *final*
	// provider/model, which can differ from the pre-call cache read key
	// (step 3) after a pre-first-byte failover. Matches the non-streaming
	// path's same-looking behavior.
	if apiKey.CacheEnabled {
		cacheKey, err := h.exactCache.CacheKey(apiKey.ProjectID, providerName, finalModel, req.Messages)
		if err == nil {
			cacheTTL := apiKey.CacheTTL
			if cacheTTL == 0 {
				cacheTTL = h.cfg.CacheTTLSeconds
			}
			if err := h.exactCache.Set(ctx, apiKey.ProjectID, cacheKey, providerName, finalModel, fullResponse, cacheTTL); err != nil {
				fmt.Printf("⚠️ Failed to cache: %v\n", err)
			} else {
				fmt.Println("💾 Cached streaming response")
			}
		}

		// Semantic cache (if enabled)
		if apiKey.SemanticCacheEnabled && h.semanticCache != nil {
			if err := h.semanticCache.Set(ctx, apiKey.ProjectID, providerName, finalModel, req.Messages, fullResponse); err != nil {
				fmt.Printf("⚠️ Failed to cache semantically: %v\n", err)
			} else {
				fmt.Println("💾 Cached streaming response (semantic)")
			}
		}
	}

	// Log request
	h.logRequest(ctx, apiKey, providerName, req, fullResponse, false, false, 0, failoverOccurred, failoverCount, requestID, customerID, customerLabels)

	fmt.Printf("✅ Post-stream processing complete (cost=$%.6f)\n", actualCost)
}
