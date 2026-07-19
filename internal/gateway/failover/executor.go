package failover

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/llm0ai/llm0/internal/gateway/providers"
	"github.com/sashabaranov/go-openai"
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

// ─────────────────────────────────────────────────────────────────────────
// Streaming failover — pre-first-byte only.
//
// Non-streaming failover (Execute, above) can freely retry: nothing has been
// sent to the client until a full response is ready. Streaming can't do
// that once bytes are flowing — a mid-stream retry would either double-bill
// the caller or splice two providers' output into one response. Every other
// gateway (LiteLLM's FallbackStreamWrapper, Portkey, OpenRouter) draws the
// same line: failover is safe up to the first byte, and never after.
//
// ExecuteStream implements exactly that: each step's stream is opened AND
// its first chunk received before we commit to it, reusing the same
// classifyError/isRetriableError matrix as Execute. The moment a step
// yields a first chunk, failover is over — the caller owns the stream from
// there and any later failure is a mid-stream error, not a retry.
// ─────────────────────────────────────────────────────────────────────────

// StreamReader is the minimal surface every provider's streaming response
// type exposes (the OpenAI SDK's *ChatCompletionStream, and each provider's
// bespoke wrapper around it). Defined here rather than in providers so both
// the executor and the HTTP handler can depend on it without a cycle.
type StreamReader interface {
	Recv() (openai.ChatCompletionStreamResponse, error)
	Close() error
}

// StreamOpener opens a live stream for one (provider, model) chain step.
// Returning an error — bad key, unknown model, connection failure, or a
// stream that ends before yielding anything — is exactly the "pre-first-byte
// failure" ExecuteStream retries past.
//
// Deliberately takes no extra timeout: it's handed the caller's own request
// context (no artificial deadline), matching the non-streaming path's
// reliance on ctx cancellation/client-disconnect rather than a fixed budget
// per attempt. A hard per-attempt deadline here would also cap the usable
// lifetime of a *successful* long-running stream (e.g. Ollama on CPU),
// which is wrong — only the wait for the first byte needs bounding, and in
// practice that's already bounded by the provider's own connect/response
// timeout under the hood.
type StreamOpener func(ctx context.Context, step FailoverStep, req providers.ChatRequest) (StreamReader, error)

// StreamFailoverResult mirrors FailoverResult for the streaming path: a
// live, already-primed Stream + its FirstChunk stand in for the full
// ChatResponse Execute() would return.
type StreamFailoverResult struct {
	Success bool

	// Stream is the live, already-primed reader for the winning step.
	// nil when Success is false.
	Stream     StreamReader
	FirstChunk openai.ChatCompletionStreamResponse

	Error error

	OriginalModel    string
	FinalModel       string
	OriginalProvider string
	FinalProvider    string
	FailoverOccurred bool
	AttemptsCount    int
	TotalLatencyMs   int

	Attempts []FailoverAttempt
}

// ExecuteStream is the streaming counterpart to Execute — see the package
// doc comment above for the safety argument. `open` is called once per
// chain step; a step "succeeds" only once open() returns AND its first
// Recv() yields a chunk.
func (e *Executor) ExecuteStream(
	ctx context.Context,
	req providers.ChatRequest,
	chain *FailoverChain,
	open StreamOpener,
) *StreamFailoverResult {
	startTime := time.Now()

	result := &StreamFailoverResult{
		OriginalModel: req.Model,
		Attempts:      []FailoverAttempt{},
	}

	steps := []FailoverStep{}
	if chain != nil {
		steps = chain.Steps
	}

	if len(steps) == 0 {
		providerName := e.detectProviderForModel(req.Model)
		if providerName == "" {
			result.Error = fmt.Errorf("no provider found for model: %s", req.Model)
			return result
		}
		steps = []FailoverStep{
			{Provider: providerName, Model: req.Model, ProviderName: providerName},
		}
	}

	if len(steps) > e.maxAttempts {
		steps = steps[:e.maxAttempts]
	}

	for i, step := range steps {
		if i == 0 {
			result.OriginalProvider = step.Provider
		}

		attempt := e.attemptStream(ctx, step, req, open)
		result.Attempts = append(result.Attempts, attempt.log)
		result.AttemptsCount++

		if attempt.success {
			result.Success = true
			result.Stream = attempt.stream
			result.FirstChunk = attempt.firstChunk
			result.FinalModel = step.Model
			result.FinalProvider = step.Provider
			result.FailoverOccurred = i > 0
			result.TotalLatencyMs = int(time.Since(startTime).Milliseconds())
			return result
		}

		if !e.isRetriableError(attempt.log) {
			result.Error = attempt.log.Error
			result.FinalProvider = step.Provider
			result.FinalModel = step.Model
			result.TotalLatencyMs = int(time.Since(startTime).Milliseconds())
			return result
		}

		fmt.Printf("⚠️  %s failed pre-first-byte (%s), trying next provider...\n",
			step.ProviderName, attempt.log.TriggerReason)
	}

	lastAttempt := result.Attempts[len(result.Attempts)-1]
	result.Error = fmt.Errorf("all providers failed: %w", lastAttempt.Error)
	result.FinalProvider = lastAttempt.Provider
	result.FinalModel = lastAttempt.Model
	result.TotalLatencyMs = int(time.Since(startTime).Milliseconds())

	return result
}

// streamAttempt is attemptStream's result — the streaming analogue of
// attemptRequest's plain FailoverAttempt, plus the live stream + first
// chunk on success (which don't fit in FailoverAttempt's log-only shape).
type streamAttempt struct {
	success    bool
	stream     StreamReader
	firstChunk openai.ChatCompletionStreamResponse
	log        FailoverAttempt
}

// attemptStream opens one chain step's stream and primes it (waits for the
// first chunk). On any failure it closes the stream (if one was opened) so
// a step that opens successfully but fails on the first Recv doesn't leak
// the underlying connection.
func (e *Executor) attemptStream(
	ctx context.Context,
	step FailoverStep,
	req providers.ChatRequest,
	open StreamOpener,
) streamAttempt {
	startTime := time.Now()
	log := FailoverAttempt{
		Provider:  step.Provider,
		Model:     step.Model,
		StartTime: startTime,
	}

	stream, err := open(ctx, step, req)
	var firstChunk openai.ChatCompletionStreamResponse
	if err == nil {
		firstChunk, err = stream.Recv()
		if err != nil {
			stream.Close()
		}
	}

	log.LatencyMs = int(time.Since(startTime).Milliseconds())

	if err != nil {
		log.Success = false
		log.Error = err
		log.ErrorMessage = err.Error()
		log.TriggerReason = e.classifyError(err, ctx)
		log.StatusCode = e.extractStatusCode(err)
		return streamAttempt{success: false, log: log}
	}

	log.Success = true
	return streamAttempt{success: true, stream: stream, firstChunk: firstChunk, log: log}
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
