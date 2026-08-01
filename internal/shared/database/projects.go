package database

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"

	"github.com/llm0ai/llm0/internal/shared/models"
)

// ============================================================================
// Projects Repository (admin surface)
//
// Backs the admin REST API (internal/gateway/admin) — the Go/SQL successor
// to the project-level commands in scripts/manage_limits.sh. Not on the hot
// request path, so there's no caching here; auth.Validator has its own
// Redis-cached read of the columns the request path actually needs.
//
// Note: a project's cache/cap settings are cached per-API-key in Redis
// (CachedAPIKey, see auth/validator.go). A write here is visible to new
// lookups immediately but existing cache entries still reflect the old
// values until CacheTTLSeconds/HotKeyCacheTTL expires.
// ============================================================================

const projectColumns = `
	id, user_id, name, monthly_cap_usd, current_month_spend_usd,
	spend_reset_at, cache_enabled, semantic_cache_enabled,
	semantic_threshold, cache_ttl_seconds, is_active, created_at, updated_at
`

// scanProject reads one projectColumns row into a models.Project.
func scanProject(scan func(...any) error) (*models.Project, error) {
	p := &models.Project{}
	err := scan(
		&p.ID, &p.UserID, &p.Name, &p.MonthlyCap, &p.CurrentMonthSpend,
		&p.SpendResetAt, &p.CacheEnabled, &p.SemanticCacheEnabled,
		&p.SemanticThreshold, &p.CacheTTL, &p.IsActive,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// CreateProject inserts a new project owned by userID. monthlyCapUSD is
// optional — a nil value falls back to the schema's own default ($20).
func (db *DB) CreateProject(ctx context.Context, userID uuid.UUID, name string, monthlyCapUSD *float64) (*models.Project, error) {
	query := `
		INSERT INTO projects (user_id, name, monthly_cap_usd)
		VALUES ($1, $2, COALESCE($3, 20))
		RETURNING ` + projectColumns

	row := db.QueryRowContext(ctx, query, userID, name, monthlyCapUSD)
	project, err := scanProject(row.Scan)
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}
	return project, nil
}

// GetProject returns a single project by ID, or sql.ErrNoRows if it doesn't
// exist.
func (db *DB) GetProject(ctx context.Context, id uuid.UUID) (*models.Project, error) {
	query := `SELECT ` + projectColumns + ` FROM projects WHERE id = $1`

	row := db.QueryRowContext(ctx, query, id)
	project, err := scanProject(row.Scan)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get project %s: %w", id, err)
	}
	return project, nil
}

// ListProjects returns every project, optionally filtered to one owner.
// userID is nil for the admin "list everything" view.
func (db *DB) ListProjects(ctx context.Context, userID *uuid.UUID) ([]models.Project, error) {
	query := `SELECT ` + projectColumns + ` FROM projects`
	args := []any{}
	if userID != nil {
		query += ` WHERE user_id = $1`
		args = append(args, *userID)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list projects: %w", err)
	}
	defer rows.Close()

	projects := make([]models.Project, 0)
	for rows.Next() {
		p, err := scanProject(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan project: %w", err)
		}
		projects = append(projects, *p)
	}
	return projects, rows.Err()
}

// ProjectPatch is a partial update for a project's mutable settings — the
// PATCH /v1/admin/projects/:id body. Nil fields are left unchanged, so a
// caller can e.g. flip is_active without having to resend the cache config.
type ProjectPatch struct {
	Name                 *string
	MonthlyCapUSD        *float64
	CacheEnabled         *bool
	SemanticCacheEnabled *bool
	SemanticThreshold    *float64
	CacheTTLSeconds      *int
	IsActive             *bool
}

// UpdateProject applies patch to project id and returns the row as it now
// stands. Returns sql.ErrNoRows if the project doesn't exist.
func (db *DB) UpdateProject(ctx context.Context, id uuid.UUID, patch *ProjectPatch) (*models.Project, error) {
	query := `
		UPDATE projects SET
			name                   = COALESCE($2, name),
			monthly_cap_usd        = COALESCE($3, monthly_cap_usd),
			cache_enabled          = COALESCE($4, cache_enabled),
			semantic_cache_enabled = COALESCE($5, semantic_cache_enabled),
			semantic_threshold     = COALESCE($6, semantic_threshold),
			cache_ttl_seconds      = COALESCE($7, cache_ttl_seconds),
			is_active              = COALESCE($8, is_active),
			updated_at = NOW()
		WHERE id = $1
		RETURNING ` + projectColumns

	row := db.QueryRowContext(ctx, query, id,
		patch.Name, patch.MonthlyCapUSD, patch.CacheEnabled, patch.SemanticCacheEnabled,
		patch.SemanticThreshold, patch.CacheTTLSeconds, patch.IsActive,
	)
	project, err := scanProject(row.Scan)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update project %s: %w", id, err)
	}
	return project, nil
}
