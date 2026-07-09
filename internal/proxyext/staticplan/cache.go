package staticplan

import "sync"

// ResultCache is a fixed-capacity in-memory cache mapping content hashes to
// PlanResults. When capacity is exceeded, the entire cache is cleared
// (simple eviction sufficient for our use case).
type ResultCache struct {
	mu  sync.Mutex
	max int
	m   map[string]PlanResult
}

// NewResultCache creates a cache with the given maximum entry count.
func NewResultCache(max int) *ResultCache {
	if max <= 0 {
		max = 256
	}
	return &ResultCache{max: max, m: make(map[string]PlanResult, max)}
}

// Get returns the cached result for hash, if present.
func (c *ResultCache) Get(hash string) (PlanResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[hash]
	return v, ok
}

// Set stores result under hash. Clears the cache when capacity is exceeded.
func (c *ResultCache) Set(hash string, result PlanResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		c.m = make(map[string]PlanResult, c.max)
	}
	c.m[hash] = result
}
