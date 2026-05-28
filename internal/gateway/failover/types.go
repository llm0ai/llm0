package failover

import (
	"context"
	"time"

	"github.com/llm0ai/llm0/internal/gateway/providers"
)

// Provider interface that all LLM providers must implement
type Provider interface {
	ChatCompletion(ctx context.Context, req providers.ChatRequest) (*providers.ChatResponse, error)
	ValidateModel(model string) bool
}

// ProviderFactory creates a provider instance
type ProviderFactory func() Provider

// FailoverChain represents a sequence of (provider, model) pairs to try
type FailoverChain struct {
	Steps []FailoverStep
}

// FailoverStep represents a single step in the failover chain
type FailoverStep struct {
	Provider     string
	Model        string
	ProviderName string // Human-readable name for logging
}

// FailoverResult contains the result of a failover attempt
type FailoverResult struct {
	// Success indicates if the request succeeded (possibly after failover)
	Success bool

	// Response from the successful attempt
	Response *providers.ChatResponse

	// Error from the final attempt (if all failed)
	Error error

	// Metadata
	OriginalModel    string
	FinalModel       string
	OriginalProvider string
	FinalProvider    string
	FailoverOccurred bool
	AttemptsCount    int
	TotalLatencyMs   int

	// Attempt details for logging
	Attempts []FailoverAttempt
}

// FailoverAttempt tracks a single attempt in the failover chain
type FailoverAttempt struct {
	Provider      string
	Model         string
	StartTime     time.Time
	LatencyMs     int
	StatusCode    int // HTTP status code (if available)
	Error         error
	ErrorMessage  string
	TriggerReason string // "rate_limit", "timeout", "server_error", "connection_error"
	Success       bool
	SkipReason    string                  // Why this step was skipped (e.g., "no_api_key")
	Response      *providers.ChatResponse // Response from successful attempt
}

// TriggerReason constants
const (
	TriggerRateLimit       = "rate_limit"
	TriggerTimeout         = "timeout"
	TriggerServerError     = "server_error"
	TriggerConnectionError = "connection_error"
	TriggerUnknownError    = "unknown_error"
)
