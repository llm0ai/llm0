package failover

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/llm0ai/llm0/internal/gateway/providers"
)

// Executor handles failover logic for LLM requests
type Executor struct {
	// Provider factories keyed by provider name
	providers map[string]ProviderFactory

	// Failover configuration
	timeoutPerAttempt time.Duration
	maxAttempts       int
}

// NewExecutor creates a new failover executor
func NewExecutor() *Executor {
	return &Executor{
		providers:         make(map[string]ProviderFactory),
		timeoutPerAttempt: 60 * time.Second, // 60s per attempt
		maxAttempts:       3,                // Try up to 3 providers
	}
}

// RegisterProvider registers a provider factory
func (e *Executor) RegisterProvider(name string, factory ProviderFactory) {
	e.providers[name] = factory
}

// Execute attempts a request with automatic failover
//
// Algorithm:
// 1. Try primary provider/model
// 2. If it fails with a retriable error (429, 5xx, timeout), try next in chain
// 3. Return first successful response OR final error if all fail
//
// For Free tier: Don't pass a failover chain (chain = nil), it will only try once
// For Pro tier: Pass the preset failover chain
// For Startup tier: Pass custom failover chain
func (e *Executor) Execute(
	ctx context.Context,
	req providers.ChatRequest,
	chain *FailoverChain,
) *FailoverResult {
	startTime := time.Now()

	result := &FailoverResult{
		OriginalModel:    req.Model,
		OriginalProvider: "", // Will be set on first attempt
		Attempts:         []FailoverAttempt{},
		Success:          false,
	}

	// If no chain provided, try only the requested model (Free tier)
	steps := []FailoverStep{}
	if chain != nil {
		steps = chain.Steps
	}

	// If chain is empty or doesn't match the model, create a single-step chain
	if len(steps) == 0 {
		// Detect provider from model
		providerName := e.detectProviderForModel(req.Model)
		if providerName == "" {
			result.Error = fmt.Errorf("no provider found for model: %s", req.Model)
			return result
		}

		steps = []FailoverStep{
			{Provider: providerName, Model: req.Model, ProviderName: providerName},
		}
	}

	// Limit attempts to maxAttempts
	if len(steps) > e.maxAttempts {
		steps = steps[:e.maxAttempts]
	}

	// Try each step in the chain
	for i, step := range steps {
		// Set original provider on first attempt
		if i == 0 {
			result.OriginalProvider = step.Provider
		}

		// Check if we have this provider registered
		factory, ok := e.providers[step.Provider]
		if !ok {
			// Skip this step - provider not available
			attempt := FailoverAttempt{
				Provider:   step.Provider,
				Model:      step.Model,
				StartTime:  time.Now(),
				Success:    false,
				SkipReason: fmt.Sprintf("provider_%s_not_configured", step.Provider),
			}
			result.Attempts = append(result.Attempts, attempt)
			continue
		}

		// Create provider instance
		provider := factory()

		// Attempt the request
		attempt := e.attemptRequest(ctx, provider, step, req)
		result.Attempts = append(result.Attempts, attempt)
		result.AttemptsCount++

		if attempt.Success {
			// Success! Return this response
			result.Success = true
			result.Response = attempt.Response
			result.FinalModel = step.Model
			result.FinalProvider = step.Provider
			result.FailoverOccurred = (i > 0) // Failover occurred if not first attempt
			result.TotalLatencyMs = int(time.Since(startTime).Milliseconds())
			return result
		}

		// Check if we should retry (retriable error)
		if !e.isRetriableError(attempt) {
			// Non-retriable error (e.g., invalid request) - don't try other providers
			result.Error = attempt.Error
			result.FinalProvider = step.Provider
			result.FinalModel = step.Model
			result.TotalLatencyMs = int(time.Since(startTime).Milliseconds())
			return result
		}

		// Log that we're trying next provider
		fmt.Printf("⚠️  %s failed (%s), trying next provider...\n",
			step.ProviderName, attempt.TriggerReason)
	}

	// All attempts failed
	lastAttempt := result.Attempts[len(result.Attempts)-1]
	result.Error = fmt.Errorf("all providers failed: %w", lastAttempt.Error)
	result.FinalProvider = lastAttempt.Provider
	result.FinalModel = lastAttempt.Model
	result.TotalLatencyMs = int(time.Since(startTime).Milliseconds())

	return result
}

// attemptRequest attempts a single request to a provider
func (e *Executor) attemptRequest(
	ctx context.Context,
	provider Provider,
	step FailoverStep,
	req providers.ChatRequest,
) FailoverAttempt {
	startTime := time.Now()
	attempt := FailoverAttempt{
		Provider:  step.Provider,
		Model:     step.Model,
		StartTime: startTime,
	}

	// Create context with timeout
	attemptCtx, cancel := context.WithTimeout(ctx, e.timeoutPerAttempt)
	defer cancel()

	// Update request with the model from this step
	attemptReq := req
	attemptReq.Model = step.Model

	// Make the request
	resp, err := provider.ChatCompletion(attemptCtx, attemptReq)

	attempt.LatencyMs = int(time.Since(startTime).Milliseconds())

	if err != nil {
		attempt.Success = false
		attempt.Error = err
		attempt.ErrorMessage = err.Error()
		attempt.TriggerReason = e.classifyError(err, attemptCtx)

		// Try to extract status code from error message
		attempt.StatusCode = e.extractStatusCode(err)

		return attempt
	}

	// Success
	attempt.Success = true
	attempt.Response = resp
	return attempt
}

// classifyError determines the type of error for failover decision
func (e *Executor) classifyError(err error, ctx context.Context) string {
	errMsg := strings.ToLower(err.Error())

	// Check for timeout
	if ctx.Err() == context.DeadlineExceeded {
		return TriggerTimeout
	}

	// Check for rate limit (429)
	if strings.Contains(errMsg, "429") || strings.Contains(errMsg, "rate limit") || strings.Contains(errMsg, "too many requests") {
		return TriggerRateLimit
	}

	// Check for auth/API key errors (401, 403, 400 with key errors)
	// These should trigger failover because another provider might have valid keys
	if strings.Contains(errMsg, "401") || strings.Contains(errMsg, "403") ||
		strings.Contains(errMsg, "unauthorized") || strings.Contains(errMsg, "forbidden") ||
		strings.Contains(errMsg, "api key") || strings.Contains(errMsg, "invalid api key") ||
		strings.Contains(errMsg, "authentication") {
		return TriggerServerError // Treat as server error for failover purposes
	}

	// Check for server errors (5xx)
	if strings.Contains(errMsg, "500") || strings.Contains(errMsg, "502") ||
		strings.Contains(errMsg, "503") || strings.Contains(errMsg, "504") ||
		strings.Contains(errMsg, "server error") || strings.Contains(errMsg, "internal error") {
		return TriggerServerError
	}

	// Check for connection errors
	if strings.Contains(errMsg, "connection") || strings.Contains(errMsg, "network") ||
		strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "dial") {
		return TriggerConnectionError
	}

	return TriggerUnknownError
}

// extractStatusCode extracts HTTP status code from error message
func (e *Executor) extractStatusCode(err error) int {
	errMsg := err.Error()

	// Common patterns in error messages
	patterns := []string{
		"status 429", "status 500", "status 502", "status 503", "status 504",
		"status 400", "status 401", "status 403", "status 404",
	}

	for _, pattern := range patterns {
		if strings.Contains(strings.ToLower(errMsg), pattern) {
			// Extract number after "status "
			parts := strings.Split(strings.ToLower(errMsg), "status ")
			if len(parts) > 1 {
				var code int
				fmt.Sscanf(parts[1], "%d", &code)
				if code >= 100 && code < 600 {
					return code
				}
			}
		}
	}

	return 0
}

// isRetriableError determines if an error should trigger failover
func (e *Executor) isRetriableError(attempt FailoverAttempt) bool {
	switch attempt.TriggerReason {
	case TriggerRateLimit, TriggerTimeout, TriggerServerError, TriggerConnectionError:
		return true
	case TriggerUnknownError:
		// Check for auth errors (invalid API key) - these ARE retriable
		// because the next provider might have a valid key
		if attempt.StatusCode == 401 || attempt.StatusCode == 403 {
			return true
		}
		// 404 from a provider means model not found or key invalid on that provider —
		// always try the next provider in the chain
		if attempt.StatusCode == 404 {
			return true
		}
		// Check for 400 with API key error messages
		if attempt.StatusCode == 400 {
			errMsg := strings.ToLower(attempt.ErrorMessage)
			if strings.Contains(errMsg, "api key") || strings.Contains(errMsg, "invalid") ||
				strings.Contains(errMsg, "authentication") || strings.Contains(errMsg, "unauthorized") {
				return true // API key issue - try next provider
			}
			return false // Other 400 errors are client errors
		}
		// 5xx errors are retriable
		if attempt.StatusCode >= 500 && attempt.StatusCode < 600 {
			return true
		}
		// 429 rate limit
		if attempt.StatusCode == http.StatusTooManyRequests {
			return true
		}
		// If no status code, assume retriable for now
		return attempt.StatusCode == 0
	default:
		return false
	}
}

// detectProviderForModel detects which provider a model belongs to
func (e *Executor) detectProviderForModel(model string) string {
	modelLower := strings.ToLower(model)

	// OpenAI models
	if strings.HasPrefix(modelLower, "gpt-") {
		return "openai"
	}

	// Anthropic models
	if strings.HasPrefix(modelLower, "claude-") {
		return "anthropic"
	}

	// Gemini models
	if strings.HasPrefix(modelLower, "gemini-") {
		return "google"
	}

	return ""
}
