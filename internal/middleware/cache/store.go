package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Store holds cached responses. The cache stage only talks to this
// interface, so a shared store (Redis, Valkey) can replace the in-process one.
type Store interface {
	// Get returns the value stored under key, if it hasn't expired.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// LRU is an in-process Store that evicts the least recently used entry once
// it holds max entries.
type LRU struct {
	max int
	now func() time.Time

	mu    sync.Mutex
	order *list.List // front is most recently used
	items map[string]*list.Element
}

type lruEntry struct {
	key     string
	value   []byte
	expires time.Time
}

func NewLRU(maxEntries int) *LRU {
	return &LRU{max: maxEntries, now: time.Now, order: list.New(), items: map[string]*list.Element{}}
}

func (c *LRU) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false, nil
	}
	e := el.Value.(*lruEntry)
	if !c.now().Before(e.expires) {
		c.order.Remove(el)
		delete(c.items, key)
		return nil, false, nil
	}
	c.order.MoveToFront(el)
	return e.value, true, nil
}

func (c *LRU) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		el.Value = &lruEntry{key, value, c.now().Add(ttl)}
		c.order.MoveToFront(el)
		return nil
	}
	c.items[key] = c.order.PushFront(&lruEntry{key, value, c.now().Add(ttl)})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*lruEntry).key)
	}
	return nil
}

// Len returns the number of entries, including expired ones not yet evicted.
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
