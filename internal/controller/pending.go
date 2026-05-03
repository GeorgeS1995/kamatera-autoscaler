package controller

import (
	"sync"
	"time"
)

// PendingTracker tracks node creations that are in flight (the VM has been
// requested from Kamatera but is not yet visible in the K8s API). Entries
// expire after a TTL even if the goroutine that created them dies, so a
// crashed scale-up cannot permanently poison the tracker.
type PendingTracker struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]*entry // key: node name
	now     func() time.Time
}

type entry struct {
	pool      string
	createdAt time.Time
}

// NewPendingTracker constructs a tracker. ttl bounds how long an in-flight entry
// is honored; pick a value comfortably larger than expected VM-ready latency.
func NewPendingTracker(ttl time.Duration) *PendingTracker {
	return &PendingTracker{
		ttl:     ttl,
		entries: map[string]*entry{},
		now:     time.Now,
	}
}

// Add records that a new node is being created for pool. The provided nodeName
// is the unique key. Returns false if the name already exists.
func (p *PendingTracker) Add(pool, nodeName string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	if _, exists := p.entries[nodeName]; exists {
		return false
	}
	p.entries[nodeName] = &entry{pool: pool, createdAt: p.now()}
	return true
}

// Remove drops the entry by node name. Idempotent.
func (p *PendingTracker) Remove(nodeName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, nodeName)
}

// Count returns the number of unexpired in-flight entries for the given pool.
func (p *PendingTracker) Count(pool string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked()
	n := 0
	for _, e := range p.entries {
		if e.pool == pool {
			n++
		}
	}
	return n
}

func (p *PendingTracker) gcLocked() {
	now := p.now()
	for k, e := range p.entries {
		if now.Sub(e.createdAt) > p.ttl {
			delete(p.entries, k)
		}
	}
}
