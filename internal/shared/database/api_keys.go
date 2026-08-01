package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/llm0ai/llm0/internal/shared/models"
	"github.com/llm0ai/llm0/internal/shared/redis"
)

// ============================================================================
// API Keys Repository (admin surface)
//
// The Go/SQL successor to scripts/create_api_key.sh and the key-management
// commands in scripts/manage_limits.sh. The hashing scheme MUST stay in
// sync with auth.Validator.validateFromDatabase: bcrypt(hex(sha256(raw))).
// bcrypt alone can't hash our 69-char raw key (its 72-byte input limit), so
// we sha256 first — see the comment there for the full rationale.
// ============================================================================

// apiKeyPrefixLen mirrors the "first N chars + ..." shown in the dashboard
// and used by auth.Validator to narrow the DB lookup before the bcrypt
// comparison. Keep this in sync with validator.go's prefix slicing.
const apiKeyPrefixLen = 15

const apiKeyColumns = `
	id, project_id, key_prefix, name, rate_limit_per_minute,
	is_active, last_used_at, created_at, updated_at
`

// scanAPIKey reads one apiKeyColumns row into a models.APIKey. KeyHash is
// deliberately never selected by any admin query — there's no legitimate
// reason for it to leave the database once written.
func scanAPIKey(scan func(...any) error) (*models.APIKey, error) {
	k := &models.APIKey{}
	err := scan(
		&k.ID, &k.ProjectID, &k.KeyPrefix, &k.Name, &k.RateLimitPerMinute,
		&k.IsActive, &k.LastUsedAt, &k.CreatedAt, &k.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return k, nil
}

// CreateAPIKey generates a new raw key, stores only its bcrypt hash, and
// returns both the raw key and the stored row. The raw key is NOT
// recoverable afterwards — this is the one and only time it's readable.
func (db *DB) CreateAPIKey(ctx context.Context, projectID uuid.UUID, name string, rateLimitPerMinute int) (rawKey string, key *models.APIKey, err error) {
	raw, err := generateRawAPIKey()
	if err != nil {
		return "", nil, fmt.Errorf("failed to generate API key: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(redis.HashSecret(raw)), bcrypt.DefaultCost)
	if err != nil {
		return "", nil, fmt.Errorf("failed to hash API key: %w", err)
	}

	if rateLimitPerMinute <= 0 {
		rateLimitPerMinute = 60 // mirrors the schema column default
	}
	prefix := raw[:apiKeyPrefixLen] + "..."

	query := `
		INSERT INTO api_keys (project_id, key_hash, key_prefix, name, rate_limit_per_minute)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING ` + apiKeyColumns

	row := db.QueryRowContext(ctx, query, projectID, string(hash), prefix, name, rateLimitPerMinute)
	created, err := scanAPIKey(row.Scan)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create API key: %w", err)
	}
	return raw, created, nil
}

// generateRawAPIKey returns a key in the format auth.Validator expects:
// "llm0_live_" followed by 64 hex chars (32 cryptographically random bytes).
func generateRawAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("failed to read random bytes: %w", err)
	}
	return "llm0_live_" + hex.EncodeToString(buf), nil
}

// ListAPIKeys returns every key (active or not) for a project, newest first.
func (db *DB) ListAPIKeys(ctx context.Context, projectID uuid.UUID) ([]models.APIKey, error) {
	query := `SELECT ` + apiKeyColumns + ` FROM api_keys WHERE project_id = $1 ORDER BY created_at DESC`

	rows, err := db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}
	defer rows.Close()

	keys := make([]models.APIKey, 0)
	for rows.Next() {
		k, err := scanAPIKey(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan API key: %w", err)
		}
		keys = append(keys, *k)
	}
	return keys, rows.Err()
}

// UpdateAPIKey applies a partial update — rate limit and/or active flag —
// and returns the row as it now stands. Returns sql.ErrNoRows if the key
// doesn't exist. As with UpdateProject, an in-flight Redis-cached key keeps
// its old settings until the cache TTL expires.
func (db *DB) UpdateAPIKey(ctx context.Context, id uuid.UUID, rateLimitPerMinute *int, isActive *bool) (*models.APIKey, error) {
	query := `
		UPDATE api_keys SET
			rate_limit_per_minute = COALESCE($2, rate_limit_per_minute),
			is_active             = COALESCE($3, is_active),
			updated_at = NOW()
		WHERE id = $1
		RETURNING ` + apiKeyColumns

	row := db.QueryRowContext(ctx, query, id, rateLimitPerMinute, isActive)
	key, err := scanAPIKey(row.Scan)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("failed to update API key %s: %w", id, err)
	}
	return key, nil
}
