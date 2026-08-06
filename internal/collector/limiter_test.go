package collector

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func concurrentAccepted(total, workers int, attempt func(int) bool) int64 {
	jobs := make(chan int)
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(workers)
	done.Add(workers)
	var accepted atomic.Int64
	for range workers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			for index := range jobs {
				if attempt(index) {
					accepted.Add(1)
				}
			}
		}()
	}
	ready.Wait()
	close(start)
	for index := range total {
		jobs <- index
	}
	close(jobs)
	done.Wait()
	return accepted.Load()
}

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

func TestConcurrentInstanceLimiterConsumesExactBurst(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	handler := New(Options{Clock: &fixedClock{now: now}})
	var unexpected atomic.Int64
	consumed := concurrentAccepted(1_000, 32, func(_ int) bool {
		req := httptest.NewRequest(http.MethodPost, PageViewsPath, strings.NewReader("invalid"))
		req.Header.Set("Content-Type", "text/plain")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		switch resp.Code {
		case http.StatusBadRequest:
			return true
		case http.StatusTooManyRequests:
			return false
		default:
			unexpected.Add(1)
			return false
		}
	})
	if unexpected.Load() != 0 {
		t.Errorf("unexpected statuses = %d, want 0", unexpected.Load())
	}
	if consumed != 500 {
		t.Errorf("instance burst consumed = %d, want 500", consumed)
	}
}

func TestConcurrentSiteLimiterConsumesExactSharedKeyBurst(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newKeyedLimiter(50, 200)
	accepted := concurrentAccepted(1_000, 32, func(_ int) bool {
		return limiter.allow("shared-site", now)
	})
	if accepted != 200 {
		t.Errorf("accepted = %d, want exact site burst 200", accepted)
	}
	if len(limiter.buckets) != 1 {
		t.Errorf("site buckets = %d, want 1", len(limiter.buckets))
	}
}

func TestConcurrentSourceLimiterConsumesExactSharedKeyBurst(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newSourceLimiter()
	var key [32]byte
	accepted := concurrentAccepted(1_000, 32, func(_ int) bool {
		return limiter.allow(key, now)
	})
	if accepted != 50 {
		t.Errorf("accepted = %d, want exact source burst 50", accepted)
	}
	if len(limiter.entries) != 1 {
		t.Errorf("source entries = %d, want 1", len(limiter.entries))
	}
}

func TestConcurrentDistinctSourcesStayBoundedDuringEviction(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := newSourceLimiter()
	const attempts = 12_000
	accepted := concurrentAccepted(attempts, 32, func(index int) bool {
		var key [32]byte
		binary.BigEndian.PutUint64(key[:8], uint64(index+1)) // #nosec G115 -- index is non-negative and bounded by attempts.
		return limiter.allow(key, now)
	})
	if accepted != attempts {
		t.Errorf("accepted = %d, want every distinct source %d", accepted, attempts)
	}
	if len(limiter.entries) != 10_000 {
		t.Errorf("source entries = %d, want cap 10000", len(limiter.entries))
	}
	if limiter.evictionCount() != 2_000 {
		t.Errorf("evictions = %d, want 2000", limiter.evictionCount())
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
