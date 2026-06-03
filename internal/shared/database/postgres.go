package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
	"github.com/llm0ai/llm0/internal/shared/config"
)

// DB wraps database connection
type DB struct {
	*sql.DB

	// In-memory hot caches. Kept on the DB struct because they cache the
	// results of DB reads and must be invalidated by DB writes.
	limitCache *customerLimitCache
	tiersCache *customerTiersCache
}

// NewPostgresDB creates a new PostgreSQL connection
func NewPostgresDB(cfg *config.Config) (*DB, error) {
	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Connection pool settings
	db.SetMaxOpenConns(100)
	db.SetMaxIdleConns(50) // Increased from 25 for better concurrency
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(1 * time.Minute)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	fmt.Printf("✅ PostgreSQL connected (max_conns=100, idle=25)\n")

	limitTTL := time.Duration(cfg.CustomerLimitCacheTTLSeconds) * time.Second
	if limitTTL <= 0 {
		limitTTL = 60 * time.Second
	}

	return &DB{
		DB:         db,
		limitCache: newCustomerLimitCache(limitTTL),
		tiersCache: newCustomerTiersCache(limitTTL),
	}, nil
}

// HealthCheck checks if database is healthy
func (db *DB) HealthCheck(ctx context.Context) error {
	return db.PingContext(ctx)
}
