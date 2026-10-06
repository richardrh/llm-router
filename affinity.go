package main

import (
	"sync"
	"time"
)

// maxAffinityEntries bounds memory for a long-running router. As with sticky
// routing, eviction is approximate: the map is cleared wholesale once it grows
// past the cap.
const maxAffinityEntries = 10_000

// cacheAffinity remembers which upstream last held a warm prompt cache for a
// given conversation prefix.
//
// Sticky routing already assumes that whichever target served one turn will
// serve the next, which keeps a cache warm as a side effect of pinning. This
// makes the assumption observable instead. Usage reports say whether the prefix
// was read from cache or written to it, so a router that has lost its pin — a
// new session, or a target that failed and was replaced — can send the
// conversation back to the cache that actually holds it rather than to a
// weighted coin toss.
//
// It is deliberately a preference, not a strategy: it is consulted only when no
// session pin applies, and it never overrides weights that point elsewhere, only
// breaks ties among targets that are equally eligible.
type cacheAffinity struct {
	ttl   time.Duration
	limit int

	mu sync.Mutex
	// entries maps a conversation fingerprint to the upstreams known to hold
	// its prefix, each with the time that was last confirmed.
	entries map[string]map[string]time.Time
}

func newCacheAffinity(ttl time.Duration, limit int) *cacheAffinity {
	return &cacheAffinity{ttl: ttl, limit: limit, entries: make(map[string]map[string]time.Time)}
}

// observe records that an upstream holds this prefix, based on what the provider
// reported. A cache read proves the prefix was already there; a cache write
// proves it is there now, for the next turn. Neither means the provider is not
// caching this traffic, and then nothing is recorded — an upstream that never
// caches must not attract traffic on the strength of a cache it does not have.
func (a *cacheAffinity) observe(fingerprint, upstream string, u Usage, now time.Time) {
	if a == nil || fingerprint == "" {
		return
	}
	if u.CacheReadTokens == 0 && u.CacheWriteTokens == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	byUpstream, ok := a.entries[fingerprint]
	if !ok {
		if len(a.entries) >= a.limit {
			a.entries = make(map[string]map[string]time.Time, a.limit/4)
		}
		byUpstream = make(map[string]time.Time, 2)
		a.entries[fingerprint] = byUpstream
	}
	byUpstream[upstream] = now
}

// warm returns the upstreams still holding this prefix. An expired entry is
// dropped on sight, because a provider's cache lifetime is what makes the
// observation meaningful: routing to a target whose cache has since expired
// costs a cache write rather than saving one.
func (a *cacheAffinity) warm(fingerprint string, now time.Time) map[string]bool {
	if a == nil || fingerprint == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	byUpstream, ok := a.entries[fingerprint]
	if !ok {
		return nil
	}
	var out map[string]bool
	for name, seen := range byUpstream {
		if now.Sub(seen) > a.ttl {
			delete(byUpstream, name)
			continue
		}
		if out == nil {
			out = make(map[string]bool, len(byUpstream))
		}
		out[name] = true
	}
	if len(byUpstream) == 0 {
		delete(a.entries, fingerprint)
	}
	return out
}

// tracked reports the number of prefixes being remembered, for tests.
func (a *cacheAffinity) tracked() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}
