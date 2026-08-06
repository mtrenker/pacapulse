package collector

import (
	"testing"
	"time"
)

func TestTokenBucketBurstAndRefill(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var b bucket
	for i := 0; i < 5; i++ {
		if !b.allow(now, 2, 5) {
			t.Fatalf("burst request %d denied", i+1)
		}
	}
	if b.allow(now, 2, 5) {
		t.Fatal("request beyond burst accepted")
	}
	now = now.Add(500 * time.Millisecond)
	if !b.allow(now, 2, 5) {
		t.Fatal("one refilled token denied")
	}
	if b.allow(now, 2, 5) {
		t.Fatal("accepted without another refill")
	}
}

func TestSourceLimiterExpiryLRUAndEvictionCounter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newSourceLimiter()
	limiter.max = 2
	limiter.idle = 10 * time.Minute
	var first, second, third [32]byte
	first[0], second[0], third[0] = 1, 2, 3
	if !limiter.allow(first, now) || !limiter.allow(second, now) {
		t.Fatal("initial sources denied")
	}
	// Touch first, making second the least recently used.
	if !limiter.allow(first, now.Add(time.Second)) {
		t.Fatal("existing source denied")
	}
	if !limiter.allow(third, now.Add(2*time.Second)) {
		t.Fatal("new source denied")
	}
	if limiter.evictionCount() != 1 {
		t.Errorf("evictions = %d, want 1", limiter.evictionCount())
	}
	if _, ok := limiter.entries[second]; ok {
		t.Error("least-recently-used source was not evicted")
	}
	if _, ok := limiter.entries[first]; !ok {
		t.Error("recent source was evicted")
	}

	limiter.allow(third, now.Add(10*time.Minute+3*time.Second))
	if _, ok := limiter.entries[first]; ok {
		t.Error("idle source did not expire")
	}
	if limiter.evictionCount() != 1 {
		t.Error("expiry must not increment eviction counter")
	}
}

func TestSourceLimiterFixedTenThousandEntryCap(t *testing.T) {
	now := time.Now()
	limiter := newSourceLimiter()
	for index := 0; index <= 10_000; index++ {
		var key [32]byte
		key[0] = byte(index >> 8)
		key[1] = byte(index)
		if !limiter.allow(key, now) {
			t.Fatalf("new source %d denied", index)
		}
	}
	if len(limiter.entries) != 10_000 {
		t.Errorf("entries = %d, want 10000", len(limiter.entries))
	}
	if limiter.evictionCount() != 1 {
		t.Errorf("evictions = %d, want 1", limiter.evictionCount())
	}
}

func TestSourceLimiterFixedBurst(t *testing.T) {
	now := time.Now()
	limiter := newSourceLimiter()
	var key [32]byte
	for i := 0; i < 50; i++ {
		if !limiter.allow(key, now) {
			t.Fatalf("burst request %d denied", i+1)
		}
	}
	if limiter.allow(key, now) {
		t.Fatal("source accepted beyond burst")
	}
	if !limiter.allow(key, now.Add(100*time.Millisecond)) {
		t.Fatal("source token did not refill")
	}
}
