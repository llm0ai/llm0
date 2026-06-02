package database

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/shared/models"
)

// ============================================================================
// Customer Limit Resolution
//
// On each request the limiter needs ONE LimitSpec to evaluate against. The
// resolver picks it from three possible sources, in precedence order:
//
//     OSS:        tier → project default → nil (unlimited)
//     Managed:    customer override → tier → project default → nil
//
// In Slice A (this file) only the OSS path is implemented. Per-customer
// overrides remain readable via GetCustomerLimit and are still respected by
// older callers; the resolver itself does not consult them yet — adding that
// step is one if-block when the managed dashboard ships (Slice B).
//
// See plans/customer-limits-tiers.md for the locked design.
// ============================================================================

// ResolveCustomerLimit returns the LimitSpec the limiter should enforce for a
// given (project, customer, tier) request, or nil when nothing is configured
// (customer is unlimited — today's behavior for OSS deploys without defaults).
//
// Hot-path cost: ZERO extra Postgres queries when both the project default
// (carried on CachedAPIKey) and the tier set (in-process cache) are warm.
// First request after restart for a project: one SELECT against customer_tiers.
//
//   apiKey: the request's authenticated key, carrying its project's default_*
//           columns. Pass nil only in admin/test contexts where no project
//           defaults should be considered.
//   tier:   value of the X-Customer-Tier header. May be empty.
func (db *DB) ResolveCustomerLimit(
	ctx context.Context,
	projectID uuid.UUID,
	tier string,
	apiKey *models.CachedAPIKey,
) (*models.LimitSpec, error) {
	// 1. Tier (owner-defined plan). Unknown slug → fall through.
	if tier != "" {
		t, err := db.GetCustomerTier(ctx, projectID, tier)
		if err != nil {
			return nil, fmt.Errorf("resolve tier %q: %w", tier, err)
		}
		if t != nil {
			spec := t.LimitSpec
			if !spec.IsEmpty() {
				return &spec, nil
			}
		}
	}

	// 2. Project default (carried on the cached API key, zero extra queries).
	if spec := apiKey.ProjectDefaultLimitSpec(); spec != nil {
		return spec, nil
	}

	// 3. Nothing configured → unlimited (today's behavior).
	return nil, nil
}

// ============================================================================
// Project Default Management (for scripts/manage_project_defaults.sh and
// the managed dashboard). Not on the hot path — these mutate the projects
// row and are read again at the next API key cache miss.
// ============================================================================

// ProjectDefaultLimits is a transport struct mirroring the projects.default_*
// columns. All fields are optional pointers: a nil means "leave unchanged"
// on SetProjectDefaults, and "no default configured" on Get.
type ProjectDefaultLimits struct {
	DailySpendLimitUSD   *float64
	MonthlySpendLimitUSD *float64
	PerRequestMaxUSD     *float64
	RequestsPerMinute    *int
	RequestsPerHour      *int
	RequestsPerDay       *int
	OnLimitBehavior      *string
	DowngradeModel       *string
}

// ToLimitSpec converts the stored defaults into a LimitSpec or returns nil if
// nothing meaningful is configured. Used by tests and admin read endpoints.
func (d *ProjectDefaultLimits) ToLimitSpec() *models.LimitSpec {
	if d == nil {
		return nil
	}
	spec := &models.LimitSpec{
		DailySpendLimitUSD:   d.DailySpendLimitUSD,
		MonthlySpendLimitUSD: d.MonthlySpendLimitUSD,
		PerRequestMaxUSD:     d.PerRequestMaxUSD,
		RequestsPerMinute:    d.RequestsPerMinute,
		RequestsPerHour:      d.RequestsPerHour,
		RequestsPerDay:       d.RequestsPerDay,
		DowngradeModel:       d.DowngradeModel,
	}
	if d.OnLimitBehavior != nil {
		spec.OnLimitBehavior = models.LimitBehavior(*d.OnLimitBehavior)
	}
	if spec.IsEmpty() {
		return nil
	}
	return spec
}

// GetProjectDefaults returns the per-customer default limits configured on a
// project. Always returns a non-nil struct; fields are nil for unset columns.
func (db *DB) GetProjectDefaults(ctx context.Context, projectID uuid.UUID) (*ProjectDefaultLimits, error) {
	const query = `
		SELECT
			default_daily_spend_limit_usd,
			default_monthly_spend_limit_usd,
			default_per_request_max_usd,
			default_requests_per_minute,
			default_requests_per_hour,
			default_requests_per_day,
			default_on_limit_behavior,
			default_downgrade_model
		FROM projects
		WHERE id = $1
	`

	var (
		daily, monthly, perReq                    sql.NullFloat64
		reqMin, reqHour, reqDay                   sql.NullInt64
		behavior, downgrade                       sql.NullString
	)
	if err := db.QueryRowContext(ctx, query, projectID).Scan(
		&daily, &monthly, &perReq,
		&reqMin, &reqHour, &reqDay,
		&behavior, &downgrade,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("project %s not found", projectID)
		}
		return nil, fmt.Errorf("failed to load project defaults: %w", err)
	}

	out := &ProjectDefaultLimits{}
	if daily.Valid {
		v := daily.Float64
		out.DailySpendLimitUSD = &v
	}
	if monthly.Valid {
		v := monthly.Float64
		out.MonthlySpendLimitUSD = &v
	}
	if perReq.Valid {
		v := perReq.Float64
		out.PerRequestMaxUSD = &v
	}
	if reqMin.Valid {
		v := int(reqMin.Int64)
		out.RequestsPerMinute = &v
	}
	if reqHour.Valid {
		v := int(reqHour.Int64)
		out.RequestsPerHour = &v
	}
	if reqDay.Valid {
		v := int(reqDay.Int64)
		out.RequestsPerDay = &v
	}
	if behavior.Valid {
		v := behavior.String
		out.OnLimitBehavior = &v
	}
	if downgrade.Valid {
		v := downgrade.String
		out.DowngradeModel = &v
	}
	return out, nil
}

// SetProjectDefaults updates the per-customer default columns on a project.
// nil-valued fields are left unchanged so callers can do partial updates
// (e.g. "only change the daily cap"). Returns ErrNoRows if the project is
// missing.
//
// Note: changes are visible to in-flight requests only after the CachedAPIKey
// Redis cache expires for any key in this project. Operators wanting an
// immediate effect should also invalidate API key caches.
func (db *DB) SetProjectDefaults(ctx context.Context, projectID uuid.UUID, in *ProjectDefaultLimits) error {
	const query = `
		UPDATE projects SET
			default_daily_spend_limit_usd   = COALESCE($2, default_daily_spend_limit_usd),
			default_monthly_spend_limit_usd = COALESCE($3, default_monthly_spend_limit_usd),
			default_per_request_max_usd     = COALESCE($4, default_per_request_max_usd),
			default_requests_per_minute     = COALESCE($5, default_requests_per_minute),
			default_requests_per_hour       = COALESCE($6, default_requests_per_hour),
			default_requests_per_day        = COALESCE($7, default_requests_per_day),
			default_on_limit_behavior       = COALESCE($8, default_on_limit_behavior),
			default_downgrade_model         = COALESCE($9, default_downgrade_model),
			updated_at = NOW()
		WHERE id = $1
	`

	res, err := db.ExecContext(ctx, query,
		projectID,
		in.DailySpendLimitUSD, in.MonthlySpendLimitUSD, in.PerRequestMaxUSD,
		in.RequestsPerMinute, in.RequestsPerHour, in.RequestsPerDay,
		in.OnLimitBehavior, in.DowngradeModel,
	)
	if err != nil {
		return fmt.Errorf("failed to set project defaults: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ClearProjectDefaults wipes ALL default_* columns on a project back to NULL.
// Used by the management script's "clear" command.
func (db *DB) ClearProjectDefaults(ctx context.Context, projectID uuid.UUID) error {
	const query = `
		UPDATE projects SET
			default_daily_spend_limit_usd   = NULL,
			default_monthly_spend_limit_usd = NULL,
			default_per_request_max_usd     = NULL,
			default_requests_per_minute     = NULL,
			default_requests_per_hour       = NULL,
			default_requests_per_day        = NULL,
			default_on_limit_behavior       = 'block',
			default_downgrade_model         = NULL,
			updated_at = NOW()
		WHERE id = $1
	`
	res, err := db.ExecContext(ctx, query, projectID)
	if err != nil {
		return fmt.Errorf("failed to clear project defaults: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountProjectCustomers returns the number of distinct customers tracked in
// customer_spend within the last `days` days for a project. Used by the
// management script to show "this default will affect N existing customers"
// before applying.
func (db *DB) CountProjectCustomers(ctx context.Context, projectID uuid.UUID, days int) (int, error) {
	if days <= 0 {
		days = 30
	}
	const query = `
		SELECT COUNT(DISTINCT customer_id)
		FROM customer_spend
		WHERE project_id = $1 AND date >= CURRENT_DATE - ($2::int) * INTERVAL '1 day'
	`
	var n int
	if err := db.QueryRowContext(ctx, query, projectID, days).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count project customers: %w", err)
	}
	return n, nil
}
