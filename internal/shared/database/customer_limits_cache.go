package database

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/llm0ai/llm0/internal/shared/models"
)

// customerLimitCache is an in-memory TTL cache for per-customer rate limit
// configuration. The limits table is read on every request that carries a
// customer ID, so hitting Postgres each time adds 1–5ms per hop. This cache
// brings that to tens of microseconds for cache hits.
//
// Invalidation:
//   - TTL-based (default 60s) — stale reads are bounded and self-healing.
//   - Explicit invalidation on UpsertCustomerLimit / DeleteCustomerLimit so
//     admin updates are visible immediately to the same gateway instance.
//
// Cross-instance consistency is eventual: other gateway replicas will pick up
// changes within one TTL window.  For 60s TTL this is almost always fine; for
// stricter semantics, deploy with a lower TTL or call the admin API against
// each instance.
//
// Negative caching is supported: when no row exists for a (project, customer)
// pair, we cache a nil *CustomerLimit so repeat "no limit configured" lookups
// don't re-query Postgres.
type customerLimitCache struct {
	entries sync.Map // map[string]*cachedLimit
	ttl     time.Duration
}

type cachedLimit struct {
	// limit is nil when the DB had no row — this is a negative cache entry.
	limit     *models.CustomerLimit
	expiresAt time.Time
}

func newCustomerLimitCache(ttl time.Duration) *customerLimitCache {
	c := &customerLimitCache{ttl: ttl}
	// Background sweeper prevents unbounded growth from unique customer IDs
	// that are never seen again. Runs every 5 minutes; cheap because it only
	// walks the map, no allocations.
	go c.sweepLoop()
	return c
}

func cacheKey(projectID uuid.UUID, customerID string) string {
	return projectID.String() + ":" + customerID
}

// get returns (limit, true) on hit, or (nil, false) on miss/expired.
// The returned *CustomerLimit may itself be nil (valid negative-cache hit).
func (c *customerLimitCache) get(projectID uuid.UUID, customerID string) (*models.CustomerLimit, bool) {
	v, ok := c.entries.Load(cacheKey(projectID, customerID))
	if !ok {
		return nil, false
	}
	cl := v.(*cachedLimit)
	if time.Now().After(cl.expiresAt) {
		// Lazy expiry; sweeper will remove stale entries too.
		c.entries.Delete(cacheKey(projectID, customerID))
		return nil, false
	}
	return cl.limit, true
}

// set caches a limit. Pass nil to cache "no limit configured".
func (c *customerLimitCache) set(projectID uuid.UUID, customerID string, limit *models.CustomerLimit) {
	c.entries.Store(cacheKey(projectID, customerID), &cachedLimit{
		limit:     limit,
		expiresAt: time.Now().Add(c.ttl),
	})
}

// invalidate removes any entry for this (project, customer).
func (c *customerLimitCache) invalidate(projectID uuid.UUID, customerID string) {
	c.entries.Delete(cacheKey(projectID, customerID))
}

// sweepLoop periodically removes expired entries. Bounded memory, cheap.
func (c *customerLimitCache) sweepLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		c.entries.Range(func(k, v interface{}) bool {
			if now.After(v.(*cachedLimit).expiresAt) {
				c.entries.Delete(k)
			}
			return true
		})
	}
}
