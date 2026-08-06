package collector

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

type bucket struct {
	tokens float64
	last   time.Time
}

func (b *bucket) allow(now time.Time, rate, burst float64) bool {
	if b.last.IsZero() {
		b.tokens = burst
		b.last = now
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(burst, b.tokens+elapsed*rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type keyedLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

func newKeyedLimiter(rate, burst float64) *keyedLimiter {
	return &keyedLimiter{rate: rate, burst: burst, buckets: make(map[string]*bucket)}
}

func (l *keyedLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{}
		l.buckets[key] = b
	}
	return b.allow(now, l.rate, l.burst)
}

type sourceEntry struct {
	key      [32]byte
	bucket   bucket
	lastSeen time.Time
	element  *list.Element
}

type sourceLimiter struct {
	mu        sync.Mutex
	entries   map[[32]byte]*sourceEntry
	lru       list.List
	max       int
	idle      time.Duration
	rate      float64
	burst     float64
	evictions atomic.Uint64
}

func newSourceLimiter() *sourceLimiter {
	return &sourceLimiter{
		entries: make(map[[32]byte]*sourceEntry),
		max:     10_000,
		idle:    10 * time.Minute,
		rate:    10,
		burst:   50,
	}
}

func (l *sourceLimiter) allow(key [32]byte, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.expire(now)
	entry := l.entries[key]
	if entry == nil {
		if len(l.entries) >= l.max {
			oldest := l.lru.Back()
			if oldest != nil {
				victim := oldest.Value.(*sourceEntry)
				delete(l.entries, victim.key)
				l.lru.Remove(oldest)
				l.evictions.Add(1)
			}
		}
		entry = &sourceEntry{key: key}
		entry.element = l.lru.PushFront(entry)
		l.entries[key] = entry
	} else {
		l.lru.MoveToFront(entry.element)
	}
	entry.lastSeen = now
	return entry.bucket.allow(now, l.rate, l.burst)
}

func (l *sourceLimiter) expire(now time.Time) {
	for {
		oldest := l.lru.Back()
		if oldest == nil {
			return
		}
		entry := oldest.Value.(*sourceEntry)
		if now.Sub(entry.lastSeen) < l.idle {
			return
		}
		delete(l.entries, entry.key)
		l.lru.Remove(oldest)
	}
}

func (l *sourceLimiter) evictionCount() uint64 { return l.evictions.Load() }
