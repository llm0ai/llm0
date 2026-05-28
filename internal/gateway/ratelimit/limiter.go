package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/shared/database"
	"github.com/llm0ai/llm0/internal/shared/models"
	redisClient "github.com/llm0ai/llm0/internal/shared/redis"
)

// ============================================================================
// Customer Rate Limiter
// ============================================================================

// Limiter handles customer-level rate limiting checks
type Limiter struct {
	redis *redisClient.Client
	db    *database.DB
}

// NewLimiter creates a new customer rate limiter
func NewLimiter(redis *redisClient.Client, db *database.DB) *Limiter {
	return &Limiter{
		redis: redis,
		db:    db,
	}
}

// CheckRequest contains all information needed for a rate limit check
type CheckRequest struct {
	ProjectID  uuid.UUID
	CustomerID string
	Model      string
	CostUSD    float64
	Labels     models.Labels // Custom attribution labels
}

// CheckResult contains the outcome of a rate limit check
type CheckResult struct {
	Allowed bool
	Reason  string // Why it was blocked (if not allowed)

	// Spend limits
	DailySpend        float64
	DailySpendLimit   *float64
	MonthlySpend      float64
	MonthlySpendLimit *float64

	// Request limits
	MinuteRequests      int
	MinuteRequestsLimit *int
	HourRequests        int
	HourRequestsLimit   *int
	DailyRequests       int
	DailyRequestsLimit  *int

	// Degradation (if applicable)
	ShouldDegrade  bool
	DowngradeModel *string

	// Headers to include in response
	Headers map[string]string
}

// Check performs a comprehensive rate limit check for a customer.
//
// Hot-path Redis strategy:
//   - All standard counters (daily/monthly spend, minute/hour/daily requests)
//     are fetched in ONE MGET round trip, not 5 serial GETs.
//   - Per-model and per-label counters (rare) are fetched with a second MGET
//     only when their limits are configured.
//
// This brings a fully-configured Check from ~5–7 Redis round trips down to
// 1–2 on the fast path.
func (l *Limiter) Check(ctx context.Context, req *CheckRequest) (*CheckResult, error) {
	// customer_limits is in-memory cached (see database.customerLimitCache).
	limit, err := l.db.GetCustomerLimit(ctx, req.ProjectID, req.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("failed to get customer limit: %w", err)
	}

	if limit == nil {
		return &CheckResult{
			Allowed: true,
			Headers: make(map[string]string),
		}, nil
	}

	result := &CheckResult{
		Allowed:           true,
		DailySpendLimit:   limit.DailySpendLimitUSD,
		MonthlySpendLimit: limit.MonthlySpendLimitUSD,
		Headers:           make(map[string]string),
	}

	// ── Single MGET for standard counters (1 round trip) ────────────────────
	// Always fetch when any cost OR request limit is configured; the extra
	// keys are free compared to the network cost of a second round trip.
	var snap *redisClient.CustomerCounterSnapshot
	if limit.HasCostLimit() || limit.HasRequestLimit() {
		snap, err = l.redis.GetCustomerCounters(ctx, req.ProjectID.String(), req.CustomerID)
		if err != nil {
			return nil, fmt.Errorf("fetch customer counters: %w", err)
		}
	}

	// 1. Cost-based limits (no Redis calls — evaluated against snapshot).
	if limit.HasCostLimit() {
		if blocked, reason := l.evaluateCostLimits(req, limit, snap, result); blocked {
			result.Allowed = false
			result.Reason = reason
			return result, nil
		}
	}

	// 2. Request-based limits (no Redis calls — evaluated against snapshot).
	if limit.HasRequestLimit() {
		if blocked, reason := l.evaluateRequestLimits(limit, snap, result); blocked {
			result.Allowed = false
			result.Reason = reason
			return result, nil
		}
	}

	// 3. Model-specific limits (separate MGET — only when configured).
	if limit.HasModelLimit(req.Model) {
		blocked, reason, err := l.checkModelLimits(ctx, req, limit, result)
		if err != nil {
			return nil, err
		}
		if blocked {
			result.Allowed = false
			result.Reason = reason
			return result, nil
		}
	}

	// 4. Label-based limits (separate MGET — only when labels present).
	if len(req.Labels) > 0 {
		blocked, reason, err := l.checkLabelLimits(ctx, req, limit, result)
		if err != nil {
			return nil, err
		}
		if blocked {
			result.Allowed = false
			result.Reason = reason
			return result, nil
		}
	}

	// 5. Per-request max cost (no network).
	if limit.PerRequestMaxUSD != nil && req.CostUSD > *limit.PerRequestMaxUSD {
		result.Allowed = false
		result.Reason = fmt.Sprintf("request cost ($%.4f) exceeds per-request limit ($%.2f)",
			req.CostUSD, *limit.PerRequestMaxUSD)
		return result, nil
	}

	return result, nil
}

// evaluateCostLimits applies daily/monthly spend caps using a pre-fetched
// counter snapshot (no Redis calls).
func (l *Limiter) evaluateCostLimits(
	req *CheckRequest,
	limit *models.CustomerLimit,
	snap *redisClient.CustomerCounterSnapshot,
	result *CheckResult,
) (blocked bool, reason string) {
	if snap == nil {
		return false, ""
	}
	result.DailySpend = snap.DailySpend
	result.MonthlySpend = snap.MonthlySpend

	if limit.DailySpendLimitUSD != nil {
		if snap.DailySpend+req.CostUSD > *limit.DailySpendLimitUSD {
			if limit.OnLimitBehavior == models.LimitBehaviorDowngrade && limit.DowngradeModel != nil {
				result.ShouldDegrade = true
				result.DowngradeModel = limit.DowngradeModel
				return false, ""
			}
			return true, fmt.Sprintf("daily spend limit exceeded (current: $%.4f, limit: $%.2f)",
				snap.DailySpend, *limit.DailySpendLimitUSD)
		}
		if snap.DailySpend >= *limit.DailySpendLimitUSD*0.8 {
			pct := (snap.DailySpend / *limit.DailySpendLimitUSD) * 100
			result.Headers["X-Warning"] = fmt.Sprintf("Customer approaching daily spend limit (%.0f%%)", pct)
		}
	}

	if limit.MonthlySpendLimitUSD != nil {
		if snap.MonthlySpend+req.CostUSD > *limit.MonthlySpendLimitUSD {
			if limit.OnLimitBehavior == models.LimitBehaviorDowngrade && limit.DowngradeModel != nil {
				result.ShouldDegrade = true
				result.DowngradeModel = limit.DowngradeModel
				return false, ""
			}
			return true, fmt.Sprintf("monthly spend limit exceeded (current: $%.4f, limit: $%.2f)",
				snap.MonthlySpend, *limit.MonthlySpendLimitUSD)
		}
		if snap.MonthlySpend >= *limit.MonthlySpendLimitUSD*0.8 {
			pct := (snap.MonthlySpend / *limit.MonthlySpendLimitUSD) * 100
			result.Headers["X-Warning"] = fmt.Sprintf("Customer approaching monthly spend limit (%.0f%%)", pct)
		}
	}

	return false, ""
}

// evaluateRequestLimits applies per-minute/hour/day request caps using a
// pre-fetched counter snapshot (no Redis calls).
func (l *Limiter) evaluateRequestLimits(
	limit *models.CustomerLimit,
	snap *redisClient.CustomerCounterSnapshot,
	result *CheckResult,
) (blocked bool, reason string) {
	if snap == nil {
		return false, ""
	}

	if limit.RequestsPerMinute != nil {
		result.MinuteRequests = snap.MinuteRequests
		result.MinuteRequestsLimit = limit.RequestsPerMinute
		if snap.MinuteRequests >= *limit.RequestsPerMinute {
			return true, fmt.Sprintf("requests per minute limit exceeded (%d/%d)",
				snap.MinuteRequests, *limit.RequestsPerMinute)
		}
	}

	if limit.RequestsPerHour != nil {
		result.HourRequests = snap.HourRequests
		result.HourRequestsLimit = limit.RequestsPerHour
		if snap.HourRequests >= *limit.RequestsPerHour {
			return true, fmt.Sprintf("requests per hour limit exceeded (%d/%d)",
				snap.HourRequests, *limit.RequestsPerHour)
		}
	}

	if limit.RequestsPerDay != nil {
		result.DailyRequests = snap.DailyRequests
		result.DailyRequestsLimit = limit.RequestsPerDay
		if snap.DailyRequests >= *limit.RequestsPerDay {
			return true, fmt.Sprintf("requests per day limit exceeded (%d/%d)",
				snap.DailyRequests, *limit.RequestsPerDay)
		}
		if snap.DailyRequests >= int(float64(*limit.RequestsPerDay)*0.8) {
			pct := (float64(snap.DailyRequests) / float64(*limit.RequestsPerDay)) * 100
			result.Headers["X-Warning"] = fmt.Sprintf("Customer approaching daily request limit (%.0f%%)", pct)
		}
	}

	return false, ""
}

// checkModelLimits verifies per-model limits
func (l *Limiter) checkModelLimits(
	ctx context.Context,
	req *CheckRequest,
	limit *models.CustomerLimit,
	result *CheckResult,
) (bool, string, error) {
	modelLimit, hasLimit := limit.GetModelLimit(req.Model)
	if !hasLimit {
		return false, "", nil // No limit for this model
	}

	// Get today's request count for this model from database
	// (We'd need to track this in a separate Redis key per model)
	// For MVP, we'll implement basic daily model tracking

	now := time.Now()
	modelKey := fmt.Sprintf("requests:customer:%s:%s:model:%s:daily:%s",
		req.ProjectID.String(), req.CustomerID, req.Model, now.Format("2006-01-02"))

	count, err := l.redis.Get(ctx, modelKey)
	if err != nil && err.Error() != "redis: nil" {
		return false, "", fmt.Errorf("failed to get model request count: %w", err)
	}

	var currentCount int
	if count != "" {
		fmt.Sscanf(count, "%d", &currentCount)
	}

	if currentCount >= modelLimit {
		return true, fmt.Sprintf("daily limit for model %s exceeded (%d/%d)",
			req.Model, currentCount, modelLimit), nil
	}

	return false, "", nil
}

// checkLabelLimits verifies label-based limits
func (l *Limiter) checkLabelLimits(
	ctx context.Context,
	req *CheckRequest,
	limit *models.CustomerLimit,
	result *CheckResult,
) (bool, string, error) {
	// Check each label against configured limits
	for _, labelKey := range req.Labels.ToLabelKeys() {
		labelLimit, hasLimit := limit.GetLabelLimit(labelKey)
		if !hasLimit {
			continue
		}

		// Get today's request count for this label
		now := time.Now()
		labelRedisKey := fmt.Sprintf("requests:customer:%s:%s:label:%s:daily:%s",
			req.ProjectID.String(), req.CustomerID, labelKey, now.Format("2006-01-02"))

		count, err := l.redis.Get(ctx, labelRedisKey)
		if err != nil && err.Error() != "redis: nil" {
			return false, "", fmt.Errorf("failed to get label request count: %w", err)
		}

		var currentCount int
		if count != "" {
			fmt.Sscanf(count, "%d", &currentCount)
		}

		if currentCount >= labelLimit {
			return true, fmt.Sprintf("daily limit for label %s exceeded (%d/%d)",
				labelKey, currentCount, labelLimit), nil
		}
	}

	return false, "", nil
}

// RecordRequest records a successful request in all tracking systems.
//
// Per-model and per-label counters use atomic INCR+EXPIRE (pipelined) rather
// than the previous racy GET→parse→SET pattern. Under concurrency the old
// path silently undercounted; this version is correct and marginally faster.
func (l *Limiter) RecordRequest(ctx context.Context, req *CheckRequest) error {
	projectID := req.ProjectID.String()
	customerID := req.CustomerID

	// 1. Track spend in Redis (daily + monthly) — atomic Lua script.
	_, _, err := l.redis.TrackCustomerSpend(ctx, projectID, customerID, req.CostUSD)
	if err != nil {
		return fmt.Errorf("failed to track customer spend: %w", err)
	}

	// 2. Track request counts in Redis (minute, hour, daily) — atomic Lua script.
	_, _, _, err = l.redis.TrackCustomerRequests(ctx, projectID, customerID, 1)
	if err != nil {
		return fmt.Errorf("failed to track customer requests: %w", err)
	}

	// 3. Track per-model requests — atomic INCR + EXPIRE, no race.
	now := time.Now()
	modelKey := fmt.Sprintf("requests:customer:%s:%s:model:%s:daily:%s",
		projectID, customerID, req.Model, now.Format("2006-01-02"))
	if _, err := l.redis.IncrWithTTL(ctx, modelKey, 24*time.Hour); err != nil {
		return fmt.Errorf("failed to track model requests: %w", err)
	}

	// 4. Track per-label requests — atomic INCR + EXPIRE per label.
	for _, labelKey := range req.Labels.ToLabelKeys() {
		labelRedisKey := fmt.Sprintf("requests:customer:%s:%s:label:%s:daily:%s",
			projectID, customerID, labelKey, now.Format("2006-01-02"))
		if _, err := l.redis.IncrWithTTL(ctx, labelRedisKey, 24*time.Hour); err != nil {
			return fmt.Errorf("failed to track label requests: %w", err)
		}
	}

	// 5. Persist to database in background (async for performance).
	go l.persistToDatabase(context.Background(), req)

	return nil
}

// persistToDatabase writes tracking data to PostgreSQL for analytics
func (l *Limiter) persistToDatabase(ctx context.Context, req *CheckRequest) {
	now := time.Now()
	spend := &models.CustomerSpend{
		ProjectID:     req.ProjectID,
		CustomerID:    req.CustomerID,
		Date:          now,
		Hour:          nil, // Daily aggregate
		TotalSpendUSD: req.CostUSD,
		RequestCount:  1,
		SpendByModel: models.ModelSpendBreakdown{
			req.Model: struct {
				Spend    float64 `json:"spend"`
				Requests int     `json:"requests"`
			}{
				Spend:    req.CostUSD,
				Requests: 1,
			},
		},
		SpendByLabel: make(models.LabelSpendBreakdown),
	}

	// Add label breakdowns
	for _, labelKey := range req.Labels.ToLabelKeys() {
		spend.SpendByLabel[labelKey] = struct {
			Spend    float64 `json:"spend"`
			Requests int     `json:"requests"`
		}{
			Spend:    req.CostUSD,
			Requests: 1,
		}
	}

	err := l.db.RecordCustomerSpend(ctx, spend)
	if err != nil {
		fmt.Printf("⚠️ Failed to persist customer spend to database: %v\n", err)
	}
}
