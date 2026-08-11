// Package lru is the response cache the gateway puts in front of the upstream
// API. It is deliberately small: a map for lookup and a slice for recency.
package lru

// Cache keeps at most Capacity entries, discarding the least recently used one
// when a new key arrives. It is not safe for concurrent use.
type Cache struct {
	capacity int
	entries  map[string]string
	order    []string // least recently used first, most recently used last
}

// New returns a cache holding at most capacity entries. A capacity below one is
// raised to one.
func New(capacity int) *Cache {
	if capacity < 1 {
		capacity = 1
	}
	return &Cache{
		capacity: capacity,
		entries:  make(map[string]string, capacity),
	}
}

// Get returns the value stored under key.
func (c *Cache) Get(key string) (string, bool) {
	value, ok := c.entries[key]
	if !ok {
		return "", false
	}
	c.touch(key)
	return value, true
}

// Put stores value under key, evicting the least recently used entry if the
// cache is full. Storing a key that is already present overwrites it.
func (c *Cache) Put(key, value string) {
	if _, ok := c.entries[key]; ok {
		c.entries[key] = value
		c.touch(key)
		return
	}

	if len(c.order) == c.capacity {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}

	c.entries[key] = value
	c.order = append(c.order, key)
}

// Len returns the number of entries currently held.
func (c *Cache) Len() int {
	return len(c.entries)
}

// Keys returns the keys held, least recently used first.
func (c *Cache) Keys() []string {
	out := make([]string, len(c.order))
	copy(out, c.order)
	return out
}

// touch moves key to the most-recently-used end of the order slice.
func (c *Cache) touch(key string) {
	for i, k := range c.order {
		if k != key {
			continue
		}
		c.order = append(c.order[:i], c.order[i+1:]...)
		c.order = append(c.order, key)
		return
	}
}
