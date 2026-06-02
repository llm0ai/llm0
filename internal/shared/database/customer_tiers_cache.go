package database

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/shared/models"
)

// customerTiersCache is an in-memory TTL cache for owner-defined customer
// tiers. Tiers are read on every request that carries an X-Customer-Tier
// header, so we cache the ENTIRE tier set per project (not per slug):
//
//   - Loading all tiers for a project is one SELECT regardless of tier count.
//   - Unknown slugs are resolved by a map lookup against the cached set
//     (no DB hit needed) → naturally negative-cached.
//   - Memory bound = O(tiers × projects), tiny in practice.
//
// Invalidation:
//   - TTL-based (default 60s) — stale reads are bounded and self-healing.
//   - Explicit invalidation on UpsertCustomerTier / DeleteCustomerTier so
//     admin updates are visible immediately to the same gateway instance.
//
// Cross-instance consistency is eventual: other gateway replicas will pick
// up changes within one TTL window — the same trade-off the override cache
// already makes.
type customerTiersCache struct {
	entries sync.Map // map[projectID.String()]*cachedTierSet
	ttl     time.Duration
}

type cachedTierSet struct {
	// bySlug holds every tier configured for the project. Missing slugs are
	// "unknown" — the resolver falls through to the project default.
	bySlug    map[string]*models.CustomerTier
	expiresAt time.Time
}

func newCustomerTiersCache(ttl time.Duration) *customerTiersCache {
	c := &customerTiersCache{ttl: ttl}
	go c.sweepLoop()
	return c
}

// get returns the cached tier set for a project, or (nil, false) on
// miss/expiry. Callers should populate via setSet on miss.
func (c *customerTiersCache) get(projectID uuid.UUID) (map[string]*models.CustomerTier, bool) {
	v, ok := c.entries.Load(projectID.String())
	if !ok {
		return nil, false
	}
	cts := v.(*cachedTierSet)
	if time.Now().After(cts.expiresAt) {
		c.entries.Delete(projectID.String())
		return nil, false
	}
	return cts.bySlug, true
}

// setSet stores the full known tier set for a project. Pass an empty map if
// the project has no tiers configured — the cache will still serve fast
// "unknown slug" responses.
func (c *customerTiersCache) setSet(projectID uuid.UUID, tiers map[string]*models.CustomerTier) {
	c.entries.Store(projectID.String(), &cachedTierSet{
		bySlug:    tiers,
		expiresAt: time.Now().Add(c.ttl),
	})
}

// invalidate evicts the full set for a project. The next lookup will re-load
// from the DB. Called from UpsertCustomerTier / DeleteCustomerTier.
func (c *customerTiersCache) invalidate(projectID uuid.UUID) {
	c.entries.Delete(projectID.String())
}

func (c *customerTiersCache) sweepLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		c.entries.Range(func(k, v interface{}) bool {
			if now.After(v.(*cachedTierSet).expiresAt) {
				c.entries.Delete(k)
			}
			return true
		})
	}
}
