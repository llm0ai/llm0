package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/shared/models"
)

// ============================================================================
// Customer Limits Repository
// ============================================================================

// GetCustomerLimit retrieves rate limit configuration for a customer.
//
// This is called on every request that carries a customer ID. Results are
// cached in-memory for db.limitCache.ttl (default 60s) to avoid hitting
// Postgres on the hot path. See customer_limits_cache.go for invalidation
// semantics.
func (db *DB) GetCustomerLimit(ctx context.Context, projectID uuid.UUID, customerID string) (*models.CustomerLimit, error) {
	// Cache hit — may be a positive (limit) or negative (nil) entry.
	if db.limitCache != nil {
		if limit, ok := db.limitCache.get(projectID, customerID); ok {
			return limit, nil
		}
	}

	query := `
		SELECT id, project_id, customer_id,
		       daily_spend_limit_usd, monthly_spend_limit_usd, per_request_max_usd,
		       requests_per_minute, requests_per_hour, requests_per_day,
		       model_limits, label_limits,
		       on_limit_behavior, downgrade_model,
		       created_at, updated_at
		FROM customer_limits
		WHERE project_id = $1 AND customer_id = $2
	`

	var limit models.CustomerLimit
	err := db.QueryRowContext(ctx, query, projectID, customerID).Scan(
		&limit.ID, &limit.ProjectID, &limit.CustomerID,
		&limit.DailySpendLimitUSD, &limit.MonthlySpendLimitUSD, &limit.PerRequestMaxUSD,
		&limit.RequestsPerMinute, &limit.RequestsPerHour, &limit.RequestsPerDay,
		&limit.ModelLimits, &limit.LabelLimits,
		&limit.OnLimitBehavior, &limit.DowngradeModel,
		&limit.CreatedAt, &limit.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		// Negative cache so repeat misses don't re-hit Postgres.
		if db.limitCache != nil {
			db.limitCache.set(projectID, customerID, nil)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get customer limit: %w", err)
	}

	if db.limitCache != nil {
		db.limitCache.set(projectID, customerID, &limit)
	}
	return &limit, nil
}

// UpsertCustomerLimit creates or updates a customer limit configuration
func (db *DB) UpsertCustomerLimit(ctx context.Context, limit *models.CustomerLimit) error {
	query := `
		INSERT INTO customer_limits (
			project_id, customer_id,
			daily_spend_limit_usd, monthly_spend_limit_usd, per_request_max_usd,
			requests_per_minute, requests_per_hour, requests_per_day,
			model_limits, label_limits,
			on_limit_behavior, downgrade_model,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())
		ON CONFLICT (project_id, customer_id)
		DO UPDATE SET
			daily_spend_limit_usd = EXCLUDED.daily_spend_limit_usd,
			monthly_spend_limit_usd = EXCLUDED.monthly_spend_limit_usd,
			per_request_max_usd = EXCLUDED.per_request_max_usd,
			requests_per_minute = EXCLUDED.requests_per_minute,
			requests_per_hour = EXCLUDED.requests_per_hour,
			requests_per_day = EXCLUDED.requests_per_day,
			model_limits = EXCLUDED.model_limits,
			label_limits = EXCLUDED.label_limits,
			on_limit_behavior = EXCLUDED.on_limit_behavior,
			downgrade_model = EXCLUDED.downgrade_model,
			updated_at = NOW()
		RETURNING id, created_at, updated_at
	`

	err := db.QueryRowContext(ctx, query,
		limit.ProjectID, limit.CustomerID,
		limit.DailySpendLimitUSD, limit.MonthlySpendLimitUSD, limit.PerRequestMaxUSD,
		limit.RequestsPerMinute, limit.RequestsPerHour, limit.RequestsPerDay,
		limit.ModelLimits, limit.LabelLimits,
		limit.OnLimitBehavior, limit.DowngradeModel,
	).Scan(&limit.ID, &limit.CreatedAt, &limit.UpdatedAt)
	if err == nil && db.limitCache != nil {
		// Invalidate so the very next Check() sees the fresh value.
		db.limitCache.invalidate(limit.ProjectID, limit.CustomerID)
	}
	return err
}

// DeleteCustomerLimit removes a customer limit configuration
func (db *DB) DeleteCustomerLimit(ctx context.Context, projectID uuid.UUID, customerID string) error {
	query := `DELETE FROM customer_limits WHERE project_id = $1 AND customer_id = $2`
	_, err := db.ExecContext(ctx, query, projectID, customerID)
	if err == nil && db.limitCache != nil {
		db.limitCache.invalidate(projectID, customerID)
	}
	return err
}

// ============================================================================
// Customer Spend Repository
// ============================================================================

// RecordCustomerSpend inserts or updates customer spend for a time window
func (db *DB) RecordCustomerSpend(ctx context.Context, spend *models.CustomerSpend) error {
	query := `
		INSERT INTO customer_spend (
			project_id, customer_id, date, hour,
			total_spend_usd, request_count,
			spend_by_model, spend_by_label,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (project_id, customer_id, date, hour)
		DO UPDATE SET
			total_spend_usd = customer_spend.total_spend_usd + EXCLUDED.total_spend_usd,
			request_count = customer_spend.request_count + EXCLUDED.request_count,
			spend_by_model = EXCLUDED.spend_by_model,
			spend_by_label = EXCLUDED.spend_by_label,
			updated_at = NOW()
		RETURNING id, created_at, updated_at
	`

	return db.QueryRowContext(ctx, query,
		spend.ProjectID, spend.CustomerID, spend.Date, spend.Hour,
		spend.TotalSpendUSD, spend.RequestCount,
		spend.SpendByModel, spend.SpendByLabel,
	).Scan(&spend.ID, &spend.CreatedAt, &spend.UpdatedAt)
}

// GetCustomerDailySpend retrieves total spend for a customer on a specific date
func (db *DB) GetCustomerDailySpend(ctx context.Context, projectID uuid.UUID, customerID string, date time.Time) (float64, error) {
	query := `
		SELECT COALESCE(SUM(total_spend_usd), 0)
		FROM customer_spend
		WHERE project_id = $1 AND customer_id = $2 AND date = $3
	`

	var total float64
	err := db.QueryRowContext(ctx, query, projectID, customerID, date).Scan(&total)
	return total, err
}

// GetCustomerMonthlySpend retrieves total spend for a customer in a specific month
func (db *DB) GetCustomerMonthlySpend(ctx context.Context, projectID uuid.UUID, customerID string, year int, month int) (float64, error) {
	query := `
		SELECT COALESCE(SUM(total_spend_usd), 0)
		FROM customer_spend
		WHERE project_id = $1
		  AND customer_id = $2
		  AND EXTRACT(YEAR FROM date) = $3
		  AND EXTRACT(MONTH FROM date) = $4
	`

	var total float64
	err := db.QueryRowContext(ctx, query, projectID, customerID, year, month).Scan(&total)
	return total, err
}

// ListCustomerSpend retrieves spend records for a customer within a date range
func (db *DB) ListCustomerSpend(ctx context.Context, projectID uuid.UUID, customerID string, startDate, endDate time.Time) ([]models.CustomerSpend, error) {
	query := `
		SELECT id, project_id, customer_id, date, hour,
		       total_spend_usd, request_count,
		       spend_by_model, spend_by_label,
		       created_at, updated_at
		FROM customer_spend
		WHERE project_id = $1
		  AND customer_id = $2
		  AND date >= $3
		  AND date <= $4
		ORDER BY date DESC, hour DESC NULLS LAST
	`

	rows, err := db.QueryContext(ctx, query, projectID, customerID, startDate, endDate)
	if err != nil {
		return nil, fmt.Errorf("failed to list customer spend: %w", err)
	}
	defer rows.Close()

	var spends []models.CustomerSpend
	for rows.Next() {
		var spend models.CustomerSpend
		err := rows.Scan(
			&spend.ID, &spend.ProjectID, &spend.CustomerID, &spend.Date, &spend.Hour,
			&spend.TotalSpendUSD, &spend.RequestCount,
			&spend.SpendByModel, &spend.SpendByLabel,
			&spend.CreatedAt, &spend.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan customer spend: %w", err)
		}
		spends = append(spends, spend)
	}

	return spends, rows.Err()
}

// GetTopSpendingCustomers retrieves the top N customers by spend for a project
func (db *DB) GetTopSpendingCustomers(ctx context.Context, projectID uuid.UUID, startDate, endDate time.Time, limit int) ([]struct {
	CustomerID    string
	TotalSpendUSD float64
	RequestCount  int
}, error) {
	query := `
		SELECT customer_id,
		       SUM(total_spend_usd) as total_spend_usd,
		       SUM(request_count) as request_count
		FROM customer_spend
		WHERE project_id = $1
		  AND date >= $2
		  AND date <= $3
		GROUP BY customer_id
		ORDER BY total_spend_usd DESC
		LIMIT $4
	`

	rows, err := db.QueryContext(ctx, query, projectID, startDate, endDate, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get top spending customers: %w", err)
	}
	defer rows.Close()

	type CustomerStat struct {
		CustomerID    string
		TotalSpendUSD float64
		RequestCount  int
	}

	var customers []CustomerStat
	for rows.Next() {
		var c CustomerStat
		err := rows.Scan(&c.CustomerID, &c.TotalSpendUSD, &c.RequestCount)
		if err != nil {
			return nil, fmt.Errorf("failed to scan customer stat: %w", err)
		}
		customers = append(customers, c)
	}

	// Convert to anonymous struct slice for return
	result := make([]struct {
		CustomerID    string
		TotalSpendUSD float64
		RequestCount  int
	}, len(customers))

	for i, c := range customers {
		result[i].CustomerID = c.CustomerID
		result[i].TotalSpendUSD = c.TotalSpendUSD
		result[i].RequestCount = c.RequestCount
	}

	return result, rows.Err()
}
