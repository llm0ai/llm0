package failover

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/shared/database"
)

// Logger logs failover events to the database
type Logger struct {
	db *database.DB
}

// NewLogger creates a new failover logger
func NewLogger(db *database.DB) *Logger {
	return &Logger{db: db}
}

// LogFailover logs a failover event to the database. Takes the raw
// occurred flag + attempt log rather than *FailoverResult so it works for
// both the non-streaming (FailoverResult) and streaming (StreamFailoverResult)
// paths, which share the same Attempts shape but aren't the same struct.
func (l *Logger) LogFailover(ctx context.Context, projectID uuid.UUID, requestID string, failoverOccurred bool, attempts []FailoverAttempt) error {
	// Only log if failover actually occurred and we have multiple attempts
	if !failoverOccurred || len(attempts) < 2 {
		return nil
	}

	// Find the original failed attempt and the successful fallback
	var originalAttempt, fallbackAttempt *FailoverAttempt

	for i := range attempts {
		if i == 0 {
			originalAttempt = &attempts[i]
		}
		if attempts[i].Success {
			fallbackAttempt = &attempts[i]
			break
		}
	}

	// If we don't have both attempts, something went wrong
	if originalAttempt == nil {
		return fmt.Errorf("no original attempt found")
	}

	// If no successful fallback, use the last attempt
	if fallbackAttempt == nil && len(attempts) > 1 {
		fallbackAttempt = &attempts[len(attempts)-1]
	}

	// If still no fallback, can't log
	if fallbackAttempt == nil {
		return fmt.Errorf("no fallback attempt found")
	}

	// Calculate total latency from all attempts
	totalLatency := 0
	for _, attempt := range attempts {
		totalLatency += attempt.LatencyMs
	}

	// Insert failover log
	query := `
		INSERT INTO failover_logs (
			id, project_id, request_id,
			original_model, original_provider,
			fallback_model, fallback_provider,
			trigger_reason, trigger_status_code, trigger_error_message,
			original_attempt_latency_ms, fallback_latency_ms, total_latency_ms,
			fallback_succeeded, fallback_error_message,
			created_at
		) VALUES (
			$1, $2, $3,
			$4, $5,
			$6, $7,
			$8, $9, $10,
			$11, $12, $13,
			$14, $15,
			NOW()
		)
	`

	var fallbackErrorMsg *string
	if !fallbackAttempt.Success && fallbackAttempt.ErrorMessage != "" {
		fallbackErrorMsg = &fallbackAttempt.ErrorMessage
	}

	var triggerStatusCode *int
	if originalAttempt.StatusCode > 0 {
		triggerStatusCode = &originalAttempt.StatusCode
	}

	_, err := l.db.ExecContext(ctx, query,
		uuid.New(),                    // id
		projectID,                     // project_id
		requestID,                     // request_id
		originalAttempt.Model,         // original_model
		originalAttempt.Provider,      // original_provider
		fallbackAttempt.Model,         // fallback_model
		fallbackAttempt.Provider,      // fallback_provider
		originalAttempt.TriggerReason, // trigger_reason
		triggerStatusCode,             // trigger_status_code
		originalAttempt.ErrorMessage,  // trigger_error_message
		originalAttempt.LatencyMs,     // original_attempt_latency_ms
		fallbackAttempt.LatencyMs,     // fallback_latency_ms
		totalLatency,                  // total_latency_ms
		fallbackAttempt.Success,       // fallback_succeeded
		fallbackErrorMsg,              // fallback_error_message
	)

	if err != nil {
		return fmt.Errorf("failed to insert failover log: %w", err)
	}

	fmt.Printf("✅ Logged failover: %s/%s -> %s/%s (reason: %s)\n",
		originalAttempt.Provider, originalAttempt.Model,
		fallbackAttempt.Provider, fallbackAttempt.Model,
		originalAttempt.TriggerReason)

	return nil
}
