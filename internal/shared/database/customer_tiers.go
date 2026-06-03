package database

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/shared/models"
)

// ============================================================================
// Customer Tiers Repository
//
// Tiers are owner-defined "plans" (e.g. 'free', 'pro') keyed by slug under a
// project. Customers carry their tier via the X-Customer-Tier header; the
// resolver looks them up here.
//
// Loads are project-scoped (one SELECT returns the full set) and cached in
// customerTiersCache so per-request resolution is a map lookup. See
// plans/customer-limits-tiers.md.
// ============================================================================

// GetCustomerTier returns the tier configured for (projectID, slug). Returns
// (nil, nil) when the slug is unknown — callers fall through to the project
// default. The full project tier set is cached on first lookup.
func (db *DB) GetCustomerTier(ctx context.Context, projectID uuid.UUID, slug string) (*models.CustomerTier, error) {
	if slug == "" {
		return nil, nil
	}

	if db.tiersCache != nil {
		if set, ok := db.tiersCache.get(projectID); ok {
			return set[slug], nil
		}
	}

	set, err := db.loadProjectTiers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if db.tiersCache != nil {
		db.tiersCache.setSet(projectID, set)
	}
	return set[slug], nil
}

// ListCustomerTiers returns all tiers configured for a project. Used by the
// admin / scripts surface; not on the hot path.
func (db *DB) ListCustomerTiers(ctx context.Context, projectID uuid.UUID) ([]models.CustomerTier, error) {
	set, err := db.loadProjectTiers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]models.CustomerTier, 0, len(set))
	for _, t := range set {
		out = append(out, *t)
	}
	return out, nil
}

// loadProjectTiers performs the actual DB read used by both the hot-path
// resolver (via the cache) and the admin list endpoint.
func (db *DB) loadProjectTiers(ctx context.Context, projectID uuid.UUID) (map[string]*models.CustomerTier, error) {
	query := `
		SELECT id, project_id, slug,
		       daily_spend_limit_usd, monthly_spend_limit_usd, per_request_max_usd,
		       requests_per_minute, requests_per_hour, requests_per_day,
		       model_limits, label_limits,
		       on_limit_behavior, downgrade_model,
		       created_at, updated_at
		FROM customer_tiers
		WHERE project_id = $1
	`

	rows, err := db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list customer tiers: %w", err)
	}
	defer rows.Close()

	set := make(map[string]*models.CustomerTier)
	for rows.Next() {
		var t models.CustomerTier
		if err := rows.Scan(
			&t.ID, &t.ProjectID, &t.Slug,
			&t.DailySpendLimitUSD, &t.MonthlySpendLimitUSD, &t.PerRequestMaxUSD,
			&t.RequestsPerMinute, &t.RequestsPerHour, &t.RequestsPerDay,
			&t.ModelLimits, &t.LabelLimits,
			&t.OnLimitBehavior, &t.DowngradeModel,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan customer tier: %w", err)
		}
		copied := t
		set[t.Slug] = &copied
	}
	return set, rows.Err()
}

// UpsertCustomerTier creates or updates a tier definition. Invalidates the
// per-project cache so the change is visible immediately on this instance.
func (db *DB) UpsertCustomerTier(ctx context.Context, tier *models.CustomerTier) error {
	query := `
		INSERT INTO customer_tiers (
			project_id, slug,
			daily_spend_limit_usd, monthly_spend_limit_usd, per_request_max_usd,
			requests_per_minute, requests_per_hour, requests_per_day,
			model_limits, label_limits,
			on_limit_behavior, downgrade_model,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())
		ON CONFLICT (project_id, slug)
		DO UPDATE SET
			daily_spend_limit_usd   = EXCLUDED.daily_spend_limit_usd,
			monthly_spend_limit_usd = EXCLUDED.monthly_spend_limit_usd,
			per_request_max_usd     = EXCLUDED.per_request_max_usd,
			requests_per_minute     = EXCLUDED.requests_per_minute,
			requests_per_hour       = EXCLUDED.requests_per_hour,
			requests_per_day        = EXCLUDED.requests_per_day,
			model_limits            = EXCLUDED.model_limits,
			label_limits            = EXCLUDED.label_limits,
			on_limit_behavior       = EXCLUDED.on_limit_behavior,
			downgrade_model         = EXCLUDED.downgrade_model,
			updated_at = NOW()
		RETURNING id, created_at, updated_at
	`

	err := db.QueryRowContext(ctx, query,
		tier.ProjectID, tier.Slug,
		tier.DailySpendLimitUSD, tier.MonthlySpendLimitUSD, tier.PerRequestMaxUSD,
		tier.RequestsPerMinute, tier.RequestsPerHour, tier.RequestsPerDay,
		tier.ModelLimits, tier.LabelLimits,
		tier.OnLimitBehavior, tier.DowngradeModel,
	).Scan(&tier.ID, &tier.CreatedAt, &tier.UpdatedAt)
	if err == nil && db.tiersCache != nil {
		db.tiersCache.invalidate(tier.ProjectID)
	}
	return err
}

// DeleteCustomerTier removes a tier definition.
func (db *DB) DeleteCustomerTier(ctx context.Context, projectID uuid.UUID, slug string) error {
	res, err := db.ExecContext(ctx,
		`DELETE FROM customer_tiers WHERE project_id = $1 AND slug = $2`,
		projectID, slug,
	)
	if err != nil {
		return err
	}
	if db.tiersCache != nil {
		db.tiersCache.invalidate(projectID)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
