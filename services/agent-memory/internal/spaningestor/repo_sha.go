package spaningestor

// repo_sha.go — per-repo current-SHA lookup with TTL cache.
//
// Evaluator iter-1 finding #7: "hardcodes EdgeInput.FromSHA to
// 'observed', while G2 fingerprints require the edge fingerprint
// preimage/from_sha to reflect the first SHA/current graph
// snapshot." The fix: thread the resolved-repo's current_head_sha
// into every observed-call edge so the G2 fingerprint key
// matches the snapshot that was current when the span was
// observed. A short TTL cache keeps the per-batch overhead at
// one Postgres query per repo per ~10s.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// SHAReader is the narrow interface the Ingestor uses to fetch
// the current head SHA for a repo. PGLookup satisfies this; the
// resolver Lookup interface is deliberately NOT extended (would
// force every existing fakeLookup to add the method).
type SHAReader interface {
	CurrentHeadSHA(ctx context.Context, repoID string) (string, error)
}

// SHACache wraps a SHAReader with a short TTL+cap cache so the
// ingestor only hits Postgres every TTL window per repo. The
// SHA is mutated only by the periodic intake job and changes at
// commit cadence (minutes-to-hours), so a few-second TTL is
// safe and bounds Postgres load.
type SHACache struct {
	reader SHAReader
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]shaCacheEntry

	hits   atomic.Int64
	misses atomic.Int64
	errs   atomic.Int64
}

type shaCacheEntry struct {
	sha       string
	expiresAt time.Time
}

// newSHACache constructs a SHACache. A nil reader is permitted
// — Get returns ("", nil) immediately, which means the ingestor
// will fall back to the documented "observed" sentinel. Tests
// can use this when they don't need a real SHA.
func newSHACache(reader SHAReader, ttl time.Duration, now func() time.Time) *SHACache {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &SHACache{
		reader:  reader,
		ttl:     ttl,
		now:     now,
		entries: make(map[string]shaCacheEntry),
	}
}

// Get returns the cached SHA for `repoID`. On a miss or expired
// entry it calls the underlying reader once, populates the
// cache, and returns the fresh SHA. Errors are surfaced to the
// caller; the cache is not poisoned with the empty value.
//
// Returns ("", nil) when the reader is nil — the caller MUST
// treat that as "fall back to documented sentinel".
func (c *SHACache) Get(ctx context.Context, repoID string) (string, error) {
	if c == nil || c.reader == nil || repoID == "" {
		return "", nil
	}
	now := c.now()

	c.mu.Lock()
	if entry, ok := c.entries[repoID]; ok && now.Before(entry.expiresAt) {
		c.mu.Unlock()
		c.hits.Add(1)
		return entry.sha, nil
	}
	c.mu.Unlock()

	sha, err := c.reader.CurrentHeadSHA(ctx, repoID)
	if err != nil {
		c.errs.Add(1)
		return "", err
	}
	c.misses.Add(1)
	if sha == "" {
		// Don't cache the empty case so we re-query when the
		// repo gets its first commit. Returning "" here lets
		// the caller use the "observed" fallback sentinel.
		return "", nil
	}
	c.mu.Lock()
	c.entries[repoID] = shaCacheEntry{sha: sha, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
	return sha, nil
}

// Snapshot returns operator-facing counters (hits, misses, errors).
func (c *SHACache) Snapshot() (hits, misses, errors int64) {
	if c == nil {
		return 0, 0, 0
	}
	return c.hits.Load(), c.misses.Load(), c.errs.Load()
}
