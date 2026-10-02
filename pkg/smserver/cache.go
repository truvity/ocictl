package smserver

import (
	"container/list"
	"os"
	"sync"
	"time"
)

// key identifies one artifact: an application and a sanitised tag.
type key struct{ app, tag string }

// diskCache is an LRU of unpacked (app, release) directories bounded by the
// bytes they hold.
type diskCache struct {
	mu    sync.Mutex
	max   int64
	used  int64
	order *list.List // front = most recently used
	items map[key]*list.Element
}

type diskEntry struct {
	key  key
	dir  string
	size int64
}

func newDiskCache(maxBytes int64) *diskCache {
	return &diskCache{max: maxBytes, order: list.New(), items: map[key]*list.Element{}}
}

// get returns the directory of k and marks it recently used.
func (c *diskCache) get(k key) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[k]
	if !ok {
		return "", false
	}

	c.order.MoveToFront(el)

	return el.Value.(*diskEntry).dir, true
}

// add records k and evicts least recently used entries (deleting their
// directories) until the cache fits. It returns how many were evicted.
func (c *diskCache) add(k key, dir string, size int64) (evicted int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[k]; ok {
		c.drop(el)
	}

	c.items[k] = c.order.PushFront(&diskEntry{key: k, dir: dir, size: size})
	c.used += size

	for c.used > c.max {
		last := c.order.Back()
		if last == nil || last.Value.(*diskEntry).key == k {
			break
		}

		c.drop(last)

		evicted++
	}

	return evicted
}

func (c *diskCache) drop(el *list.Element) {
	e := c.order.Remove(el).(*diskEntry)
	delete(c.items, e.key)
	c.used -= e.size
	_ = os.RemoveAll(e.dir)
}

func (c *diskCache) usedBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.used
}

// negativeCache remembers keys with no usable artifact for a TTL, holding at
// most max of them (oldest insertion goes first).
type negativeCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	clock func() time.Time
	order *list.List
	items map[key]*list.Element
}

type negEntry struct {
	key     key
	expires time.Time
}

func newNegativeCache(ttl time.Duration, maxEntries int, clock func() time.Time) *negativeCache {
	return &negativeCache{ttl: ttl, max: maxEntries, clock: clock, order: list.New(), items: map[key]*list.Element{}}
}

func (n *negativeCache) has(k key) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	el, ok := n.items[k]
	if !ok {
		return false
	}

	if !n.clock().Before(el.Value.(*negEntry).expires) {
		n.order.Remove(el)
		delete(n.items, k)

		return false
	}

	return true
}

func (n *negativeCache) add(k key) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if el, ok := n.items[k]; ok {
		n.order.Remove(el)
		delete(n.items, k)
	}

	for n.order.Len() >= n.max {
		oldest := n.order.Front()
		n.order.Remove(oldest)
		delete(n.items, oldest.Value.(*negEntry).key)
	}

	n.items[k] = n.order.PushBack(&negEntry{key: k, expires: n.clock().Add(n.ttl)})
}

func (n *negativeCache) len() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return len(n.items)
}
