package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Customer Rate Limiting Models
// ============================================================================

// LimitBehavior defines what happens when a customer hits their limit
type LimitBehavior string

const (
	LimitBehaviorBlock     LimitBehavior = "block"     // Return 429 error
	LimitBehaviorQueue     LimitBehavior = "queue"     // Delay request (not implemented yet)
	LimitBehaviorDowngrade LimitBehavior = "downgrade" // Use cheaper model
	LimitBehaviorWarn      LimitBehavior = "warn"      // Allow but warn
)

// ModelLimits represents per-model request limits
// Example: {"gpt-4": 100, "gpt-4o-mini": null, "claude-3-5-sonnet": 50}
// null = unlimited, number = max requests per day
type ModelLimits map[string]*int

// Value implements driver.Valuer for database storage
func (ml ModelLimits) Value() (driver.Value, error) {
	if ml == nil {
		return nil, nil
	}
	return json.Marshal(ml)
}

// Scan implements sql.Scanner for database retrieval
func (ml *ModelLimits) Scan(value interface{}) error {
	if value == nil {
		*ml = nil
		return nil
	}

	bytes, ok := value.([]byte)
	if !ok {
		return errors.New("failed to unmarshal ModelLimits value")
	}

	return json.Unmarshal(bytes, ml)
}

// LabelLimits represents per-label request limits
// Example: {"feature:chat": 1000, "feature:summarization": 100, "team:support": 500}
type LabelLimits map[string]int

// Value implements driver.Valuer for database storage
func (ll LabelLimits) Value() (driver.Value, error) {
	if ll == nil {
		return nil, nil
	}
	return json.Marshal(ll)
}

// Scan implements sql.Scanner for database retrieval
func (ll *LabelLimits) Scan(value interface{}) error {
	if value == nil {
		*ll = nil
		return nil
	}

	bytes, ok := value.([]byte)
	if !ok {
		return errors.New("failed to unmarshal LabelLimits value")
	}

	return json.Unmarshal(bytes, ll)
}

// LimitSpec is the shared cap/policy bundle that can be sourced from a
// per-customer override row (CustomerLimit), an owner-defined CustomerTier,
// or project default columns (projects.default_*). The limiter's evaluation
// path operates on *LimitSpec only — it doesn't care which source produced
// the values. See plans/customer-limits-tiers.md for the resolution rule.
type LimitSpec struct {
	// Cost-based limits
	DailySpendLimitUSD   *float64 `db:"daily_spend_limit_usd" json:"daily_spend_limit_usd"`
	MonthlySpendLimitUSD *float64 `db:"monthly_spend_limit_usd" json:"monthly_spend_limit_usd"`
	PerRequestMaxUSD     *float64 `db:"per_request_max_usd" json:"per_request_max_usd"`

	// Request-based limits
	RequestsPerMinute *int `db:"requests_per_minute" json:"requests_per_minute"`
	RequestsPerHour   *int `db:"requests_per_hour" json:"requests_per_hour"`
	RequestsPerDay    *int `db:"requests_per_day" json:"requests_per_day"`

	// Advanced limits (JSONB)
	ModelLimits ModelLimits `db:"model_limits" json:"model_limits,omitempty"`
	LabelLimits LabelLimits `db:"label_limits" json:"label_limits,omitempty"`

	// Behavior on limit
	OnLimitBehavior LimitBehavior `db:"on_limit_behavior" json:"on_limit_behavior"`
	DowngradeModel  *string       `db:"downgrade_model" json:"downgrade_model"`
}

// HasCostLimit returns true if any cost-based limit is configured
func (l *LimitSpec) HasCostLimit() bool {
	if l == nil {
		return false
	}
	return l.DailySpendLimitUSD != nil ||
		l.MonthlySpendLimitUSD != nil ||
		l.PerRequestMaxUSD != nil
}

// HasRequestLimit returns true if any request-based limit is configured
func (l *LimitSpec) HasRequestLimit() bool {
	if l == nil {
		return false
	}
	return l.RequestsPerMinute != nil ||
		l.RequestsPerHour != nil ||
		l.RequestsPerDay != nil
}

// HasModelLimit returns true if a limit exists for the specified model
func (l *LimitSpec) HasModelLimit(model string) bool {
	if l == nil || l.ModelLimits == nil {
		return false
	}
	_, exists := l.ModelLimits[model]
	return exists
}

// GetModelLimit returns the request limit for a specific model.
// Returns (limit, hasLimit).
func (l *LimitSpec) GetModelLimit(model string) (int, bool) {
	if l == nil || l.ModelLimits == nil {
		return 0, false
	}
	limit, exists := l.ModelLimits[model]
	if !exists || limit == nil {
		return 0, false
	}
	return *limit, true
}

// HasLabelLimit returns true if a limit exists for the specified label
func (l *LimitSpec) HasLabelLimit(labelKey string) bool {
	if l == nil || l.LabelLimits == nil {
		return false
	}
	_, exists := l.LabelLimits[labelKey]
	return exists
}

// GetLabelLimit returns the request limit for a specific label
func (l *LimitSpec) GetLabelLimit(labelKey string) (int, bool) {
	if l == nil || l.LabelLimits == nil {
		return 0, false
	}
	limit, exists := l.LabelLimits[labelKey]
	return limit, exists
}

// IsEmpty returns true if no cap of any kind is configured. Useful to skip
// allocating a spec when project defaults / tier rows have all-NULL columns.
func (l *LimitSpec) IsEmpty() bool {
	if l == nil {
		return true
	}
	return !l.HasCostLimit() &&
		!l.HasRequestLimit() &&
		len(l.ModelLimits) == 0 &&
		len(l.LabelLimits) == 0
}

// CustomerLimit defines a per-customer override row (project_id × customer_id).
// All cap fields are inherited via the embedded LimitSpec, so existing code
// that reads e.g. limit.DailySpendLimitUSD or calls limit.HasCostLimit()
// continues to work unchanged. Per-customer overrides are managed-cloud only
// in OSS Slice A; project defaults + tiers handle every OSS use case.
type CustomerLimit struct {
	ID         uuid.UUID `db:"id" json:"id"`
	ProjectID  uuid.UUID `db:"project_id" json:"project_id"`
	CustomerID string    `db:"customer_id" json:"customer_id"`

	LimitSpec

	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// CustomerTier is an owner-defined "plan" (e.g. 'free', 'pro', or any slug
// the owner picks). Customers carry their tier via the X-Customer-Tier
// request header. Names are NOT predefined by LLM0; the owner manages them
// via scripts/manage_tiers.sh (OSS) or the managed dashboard.
type CustomerTier struct {
	ID        uuid.UUID `db:"id" json:"id"`
	ProjectID uuid.UUID `db:"project_id" json:"project_id"`
	Slug      string    `db:"slug" json:"slug"`

	LimitSpec

	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// ============================================================================
// Customer Spend Tracking Models
// ============================================================================

// ModelSpendBreakdown represents spend per model
// Example: {"gpt-4": {"spend": 1.234, "requests": 10}, "gpt-4o-mini": {"spend": 0.005, "requests": 50}}
type ModelSpendBreakdown map[string]struct {
	Spend    float64 `json:"spend"`
	Requests int     `json:"requests"`
}

// Value implements driver.Valuer for database storage
func (msb ModelSpendBreakdown) Value() (driver.Value, error) {
	if msb == nil {
		return nil, nil
	}
	return json.Marshal(msb)
}

// Scan implements sql.Scanner for database retrieval
func (msb *ModelSpendBreakdown) Scan(value interface{}) error {
	if value == nil {
		*msb = nil
		return nil
	}

	bytes, ok := value.([]byte)
	if !ok {
		return errors.New("failed to unmarshal ModelSpendBreakdown value")
	}

	return json.Unmarshal(bytes, msb)
}

// LabelSpendBreakdown represents spend per label
// Example: {"feature:chat": {"spend": 0.5, "requests": 20}, "team:support": {"spend": 0.3, "requests": 10}}
type LabelSpendBreakdown map[string]struct {
	Spend    float64 `json:"spend"`
	Requests int     `json:"requests"`
}

// Value implements driver.Valuer for database storage
func (lsb LabelSpendBreakdown) Value() (driver.Value, error) {
	if lsb == nil {
		return nil, nil
	}
	return json.Marshal(lsb)
}

// Scan implements sql.Scanner for database retrieval
func (lsb *LabelSpendBreakdown) Scan(value interface{}) error {
	if value == nil {
		*lsb = nil
		return nil
	}

	bytes, ok := value.([]byte)
	if !ok {
		return errors.New("failed to unmarshal LabelSpendBreakdown value")
	}

	return json.Unmarshal(bytes, lsb)
}

// CustomerSpend tracks actual usage per customer per time window
type CustomerSpend struct {
	ID         uuid.UUID `db:"id"`
	ProjectID  uuid.UUID `db:"project_id"`
	CustomerID string    `db:"customer_id"`

	// Time window
	Date time.Time `db:"date"` // YYYY-MM-DD
	Hour *int      `db:"hour"` // 0-23 for hourly, null for daily

	// Aggregate metrics
	TotalSpendUSD float64 `db:"total_spend_usd"`
	RequestCount  int     `db:"request_count"`

	// Breakdowns (JSONB)
	SpendByModel ModelSpendBreakdown `db:"spend_by_model"`
	SpendByLabel LabelSpendBreakdown `db:"spend_by_label"`

	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

// IsHourly returns true if this is an hourly record (not a daily aggregate)
func (cs *CustomerSpend) IsHourly() bool {
	return cs.Hour != nil
}

// Labels represents custom attribution labels from headers
// Example: {"feature": "chat", "team": "support", "client": "agency_A"}
type Labels map[string]string

// Value implements driver.Valuer for database storage
func (l Labels) Value() (driver.Value, error) {
	if l == nil {
		return nil, nil
	}
	return json.Marshal(l)
}

// Scan implements sql.Scanner for database retrieval
func (l *Labels) Scan(value interface{}) error {
	if value == nil {
		*l = nil
		return nil
	}

	bytes, ok := value.([]byte)
	if !ok {
		return errors.New("failed to unmarshal Labels value")
	}

	return json.Unmarshal(bytes, l)
}

// ToLabelKeys converts Labels to label keys for limit lookups
// Example: {"feature": "chat", "team": "support"} → ["feature:chat", "team:support"]
func (l Labels) ToLabelKeys() []string {
	if l == nil {
		return nil
	}

	keys := make([]string, 0, len(l))
	for k, v := range l {
		keys = append(keys, k+":"+v)
	}
	return keys
}
