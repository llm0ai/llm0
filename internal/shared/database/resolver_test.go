package database

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/llm0ai/llm0/internal/shared/models"
)

func fp(v float64) *float64 { return &v }
func ip(v int) *int         { return &v }
func sp(v string) *string   { return &v }

// newTestDB returns a *DB with only the in-memory caches initialized — no
// Postgres connection. Safe for resolver unit tests because the project-
// default branch reads from CachedAPIKey (not Postgres) and the tier branch
// is short-circuited by a pre-populated tiersCache.
func newTestDB() *DB {
	return &DB{
		limitCache: newCustomerLimitCache(60 * time.Second),
		tiersCache: newCustomerTiersCache(60 * time.Second),
	}
}

func TestResolveCustomerLimit_TierBeatsProjectDefault(t *testing.T) {
	db := newTestDB()
	projectID := uuid.New()

	// Pre-populate the tier cache with one tier — avoids any DB call.
	db.tiersCache.setSet(projectID, map[string]*models.CustomerTier{
		"pro": {
			ID:        uuid.New(),
			ProjectID: projectID,
			Slug:      "pro",
			LimitSpec: models.LimitSpec{
				DailySpendLimitUSD: fp(5.00),
				OnLimitBehavior:    models.LimitBehaviorBlock,
			},
		},
	})

	// Project default also configures something — tier should still win.
	apiKey := &models.CachedAPIKey{
		ProjectID:                 projectID,
		DefaultDailySpendLimitUSD: fp(0.50), // cheap default that should NOT apply
		DefaultOnLimitBehavior:    sp("block"),
	}

	spec, err := db.ResolveCustomerLimit(context.Background(), projectID, "pro", apiKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec == nil {
		t.Fatal("expected non-nil spec")
	}
	if *spec.DailySpendLimitUSD != 5.00 {
		t.Fatalf("daily = %v, want 5.00 (tier should win over default)", spec.DailySpendLimitUSD)
	}
}

func TestResolveCustomerLimit_FallsBackToProjectDefault(t *testing.T) {
	db := newTestDB()
	projectID := uuid.New()

	// Empty tier set for the project — every tier slug is "unknown."
	db.tiersCache.setSet(projectID, map[string]*models.CustomerTier{})

	apiKey := &models.CachedAPIKey{
		ProjectID:                 projectID,
		DefaultDailySpendLimitUSD: fp(2.00),
		DefaultOnLimitBehavior:    sp("downgrade"),
		DefaultDowngradeModel:     sp("gpt-4o-mini"),
	}

	spec, err := db.ResolveCustomerLimit(context.Background(), projectID, "free", apiKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec == nil {
		t.Fatal("expected non-nil spec (project default should apply)")
	}
	if *spec.DailySpendLimitUSD != 2.00 {
		t.Fatalf("daily = %v, want 2.00", spec.DailySpendLimitUSD)
	}
	if spec.OnLimitBehavior != models.LimitBehaviorDowngrade {
		t.Fatalf("OnLimitBehavior = %q, want %q", spec.OnLimitBehavior, models.LimitBehaviorDowngrade)
	}
}

func TestResolveCustomerLimit_UnknownTierFallsThrough(t *testing.T) {
	db := newTestDB()
	projectID := uuid.New()
	// Tier set has only 'pro'.
	db.tiersCache.setSet(projectID, map[string]*models.CustomerTier{
		"pro": {Slug: "pro", LimitSpec: models.LimitSpec{DailySpendLimitUSD: fp(10)}},
	})
	apiKey := &models.CachedAPIKey{
		ProjectID:                 projectID,
		DefaultDailySpendLimitUSD: fp(2.00),
		DefaultOnLimitBehavior:    sp("block"),
	}

	// Request carries an unknown tier — should fall through to project default.
	spec, err := db.ResolveCustomerLimit(context.Background(), projectID, "platinum", apiKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec == nil {
		t.Fatal("expected project-default spec, got nil")
	}
	if *spec.DailySpendLimitUSD != 2.00 {
		t.Fatalf("daily = %v, want 2.00 (default — unknown tier should fall through)", spec.DailySpendLimitUSD)
	}
}

func TestResolveCustomerLimit_NoTierNoDefault(t *testing.T) {
	db := newTestDB()
	projectID := uuid.New()
	db.tiersCache.setSet(projectID, map[string]*models.CustomerTier{})
	apiKey := &models.CachedAPIKey{ProjectID: projectID}

	spec, err := db.ResolveCustomerLimit(context.Background(), projectID, "", apiKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec != nil {
		t.Fatalf("expected nil spec (unlimited), got %+v", spec)
	}
}

func TestResolveCustomerLimit_EmptyTierSpecFallsThrough(t *testing.T) {
	// A tier row exists but all its caps are NULL — treat it the same as an
	// unknown tier so the project default still applies. This protects
	// against a misconfigured tier silently disabling all limits.
	db := newTestDB()
	projectID := uuid.New()
	db.tiersCache.setSet(projectID, map[string]*models.CustomerTier{
		"empty": {Slug: "empty", LimitSpec: models.LimitSpec{OnLimitBehavior: models.LimitBehaviorBlock}},
	})
	apiKey := &models.CachedAPIKey{
		ProjectID:                 projectID,
		DefaultDailySpendLimitUSD: fp(2.00),
		DefaultOnLimitBehavior:    sp("block"),
	}

	spec, err := db.ResolveCustomerLimit(context.Background(), projectID, "empty", apiKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec == nil || spec.DailySpendLimitUSD == nil || *spec.DailySpendLimitUSD != 2.00 {
		t.Fatalf("expected project default to apply when tier is empty; got %+v", spec)
	}
}

func TestProjectDefaultLimits_ToLimitSpec(t *testing.T) {
	t.Run("nil returns nil", func(t *testing.T) {
		var d *ProjectDefaultLimits
		if d.ToLimitSpec() != nil {
			t.Fatal("nil receiver should return nil spec")
		}
	})
	t.Run("empty returns nil", func(t *testing.T) {
		if (&ProjectDefaultLimits{}).ToLimitSpec() != nil {
			t.Fatal("empty struct should return nil spec")
		}
	})
	t.Run("populated", func(t *testing.T) {
		d := &ProjectDefaultLimits{
			MonthlySpendLimitUSD: fp(20),
			OnLimitBehavior:      sp("downgrade"),
			DowngradeModel:       sp("gpt-4o-mini"),
			RequestsPerMinute:    ip(30),
		}
		s := d.ToLimitSpec()
		if s == nil {
			t.Fatal("expected non-nil spec")
		}
		if s.OnLimitBehavior != models.LimitBehaviorDowngrade {
			t.Fatalf("OnLimitBehavior = %q", s.OnLimitBehavior)
		}
		if s.RequestsPerMinute == nil || *s.RequestsPerMinute != 30 {
			t.Fatalf("rpm = %v", s.RequestsPerMinute)
		}
	})
}
