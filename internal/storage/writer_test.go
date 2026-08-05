package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockedWriter is a writer goroutine held inside one transaction.
//
// It is how the queue tests reach the states that only exist while a write is
// in progress: a saturated queue, a caller that gives up while waiting, and a
// shutdown that has to let an in-flight transaction finish.
type blockedWriter struct {
	// finished carries the paused page view's own result.
	finished <-chan error
	// release lets the paused transaction, and every later one, run on.
	release func()
}

func blockWriter(t *testing.T, db *Database, view PageView) *blockedWriter {
	t.Helper()

	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	db.fail.Store(&failureHook{fn: func(stage string) error {
		if stage != "ledger" {
			return nil
		}
		once.Do(func() { close(entered) })
		// Closed, this returns immediately, so later transactions are not
		// affected once the test has released the first one.
		<-gate
		return nil
	}})
	t.Cleanup(func() { db.fail.Store(nil) })

	finished := make(chan error, 1)
	go func() {
		_, err := db.RecordPageView(context.Background(), view)
		finished <- err
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer never reached the paused transaction")
	}

	var releaseOnce sync.Once
	blocked := &blockedWriter{
		finished: finished,
		release:  func() { releaseOnce.Do(func() { close(gate) }) },
	}
	t.Cleanup(blocked.release)
	return blocked
}

// waitForQueueDepth blocks until the writer's backlog is exactly want, so that
// a test asserts against a queue state it has observed rather than assumed.
func waitForQueueDepth(t *testing.T, db *Database, want int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if db.queueDepth() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue depth = %d, want %d", db.queueDepth(), want)
}

// TestFullQueueIsRejectedAndCancelledCallersAreAbandoned covers the two things
// backpressure has to do: refuse work it cannot take, immediately and with a
// retryable error, and execute nothing for a caller that gave up while queued.
func TestFullQueueIsRejectedAndCancelledCallersAreAbandoned(t *testing.T) {
	t.Parallel()

	db := newStore(t)
	inFlight := samplePageView()
	blocked := blockWriter(t, db, inFlight)

	// Fill the queue behind the paused transaction.
	queued, cancelQueued := context.WithCancel(context.Background())
	defer cancelQueued()

	var wg sync.WaitGroup
	rejections := make(chan error, writeQueueCapacity)
	for i := range writeQueueCapacity {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view := samplePageView()
			view.EventID = eventID(i + 2)
			_, err := db.RecordPageView(queued, view)
			rejections <- err
		}()
	}
	waitForQueueDepth(t, db, writeQueueCapacity)

	// One more is refused rather than queued or waited on, and the refusal is
	// immediate: the paused transaction is still holding the writer.
	overflow := samplePageView()
	overflow.EventID = eventID(1000)
	start := time.Now()
	_, err := db.RecordPageView(context.Background(), overflow)
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("RecordPageView() error = %v, want %v", err, ErrQueueFull)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a full queue waited %v before rejecting; it must refuse immediately", elapsed)
	}
	assertPrivate(t, err, overflow.EventID)

	// Every queued caller then gives up. None of them may be executed.
	cancelQueued()
	blocked.release()
	wg.Wait()
	for range writeQueueCapacity {
		if err := <-rejections; !errors.Is(err, context.Canceled) {
			t.Fatalf("queued caller error = %v, want %v", err, context.Canceled)
		}
	}
	if err := <-blocked.finished; err != nil {
		t.Fatalf("the in-flight page view failed: %v", err)
	}

	// Only the transaction that was already running was committed.
	if got := query[int](t, db, "SELECT count(*) FROM page_view_events"); got != 1 {
		t.Errorf("ledger rows = %d, want only the in-flight event", got)
	}
	if got := query[int](t, db, "SELECT page_views FROM daily_site_stats"); got != 1 {
		t.Errorf("site page views = %d, want 1", got)
	}
}

// TestCancelledCallerLeavesNoTrace is the single-request form of the same rule,
// stated where it is easy to read.
func TestCancelledCallerLeavesNoTrace(t *testing.T) {
	t.Parallel()

	db := newStore(t)
	blocked := blockWriter(t, db, samplePageView())

	ctx, cancel := context.WithCancel(context.Background())
	abandoned := make(chan error, 1)
	view := samplePageView()
	view.EventID = eventID(2)
	go func() {
		_, err := db.RecordPageView(ctx, view)
		abandoned <- err
	}()
	waitForQueueDepth(t, db, 1)

	cancel()
	if err := <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("RecordPageView() error = %v, want %v", err, context.Canceled)
	}

	blocked.release()
	if err := <-blocked.finished; err != nil {
		t.Fatalf("the in-flight page view failed: %v", err)
	}
	if got := query[int](t, db,
		"SELECT count(*) FROM page_view_events WHERE event_id = ?", view.EventID); got != 0 {
		t.Errorf("ledger rows for the abandoned event = %d, want 0", got)
	}
}

// TestVisitorKeyIsCopiedFromTheCaller covers a buffer the store does not own.
//
// The key belongs to the caller, and a caller that stops waiting — a collection
// request that timed out while queued — is free to clear or reuse it. Sharing
// the slice would be a data race, and worse than that a correctness failure:
// the bytes that landed in the deduplication table would be whatever the buffer
// happened to hold, so one visitor's identity could become another's.
func TestVisitorKeyIsCopiedFromTheCaller(t *testing.T) {
	t.Parallel()

	db := newStore(t)
	key := visitorKey(7)
	original := bytes.Clone(key)

	// The buffer is overwritten from inside the transaction, after the writer
	// has taken the request and before it inserts the deduplication row: the
	// worst moment a reusing caller could pick. Running it on the writer's own
	// goroutine is what makes the timing adversarial without making the test
	// itself the unsynchronized access — the queue handoff orders the
	// overwrite after RecordPageView copied the key.
	var once sync.Once
	db.fail.Store(&failureHook{fn: func(stage string) error {
		if stage == "ledger" {
			once.Do(func() {
				for i := range key {
					key[i] = 0xff
				}
			})
		}
		return nil
	}})
	t.Cleanup(func() { db.fail.Store(nil) })

	view := samplePageView()
	view.VisitorKey = key
	record(t, db, view)

	if got := query[int](t, db,
		"SELECT count(*) FROM daily_site_visitors WHERE visitor_key = ?", original); got != 1 {
		t.Errorf("rows for the offered key = %d, want 1: the key was not copied", got)
	}
	if got := query[int](t, db,
		"SELECT count(*) FROM daily_site_visitors WHERE visitor_key = ?", key); got != 0 {
		t.Errorf("rows for the caller's overwritten buffer = %d, want 0", got)
	}
	for _, table := range visitorTables {
		if got := query[int](t, db,
			"SELECT count(*) FROM "+table+" WHERE visitor_key = ?", original); got != 1 {
			t.Errorf("%s rows for the offered key = %d, want 1", table, got)
		}
	}
}

// TestConcurrentWritesDuringShutdown is the adversarial case: callers still
// arriving while the store is being closed, and closed twice at that.
//
// It asserts the two properties that must survive it — every call returns, and
// every call that reported success is in the ledger afterwards — rather than a
// particular interleaving, because the interleaving is the point.
func TestConcurrentWritesDuringShutdown(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	db := open(t, dir)
	addSite(t, db, testSiteID)

	const (
		callers        = 8
		viewsPerCaller = 25
	)
	var (
		mu        sync.Mutex
		committed []string
		writers   sync.WaitGroup
		warmOnce  sync.Once
	)
	// Shutdown waits until the writer is demonstrably working, so that the
	// close really does race live traffic instead of winning before the first
	// caller is scheduled.
	warm := make(chan struct{})

	for caller := range callers {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range viewsPerCaller {
				view := samplePageView()
				view.EventID = eventID(caller*viewsPerCaller + i + 1)
				view.VisitorKey = visitorKey(byte(caller))
				result, err := db.RecordPageView(context.Background(), view)
				if err != nil {
					// ErrClosed and ErrQueueFull are the expected refusals.
					// Anything else would be a failure to report cleanly.
					if !errors.Is(err, ErrClosed) && !errors.Is(err, ErrQueueFull) {
						t.Errorf("RecordPageView() error = %v, want a refusal", err)
					}
					continue
				}
				if result.Duplicate {
					t.Error("Duplicate = true for a distinct event id")
					continue
				}
				mu.Lock()
				committed = append(committed, view.EventID)
				enough := len(committed) >= callers
				mu.Unlock()
				if enough {
					warmOnce.Do(func() { close(warm) })
				}
			}
		}()
	}

	// Two shutdowns racing each other and the writers.
	var closers sync.WaitGroup
	closeErrs := make(chan error, 2)
	for range 2 {
		closers.Add(1)
		go func() {
			defer closers.Done()
			<-warm
			closeErrs <- db.Close()
		}()
	}

	// Every caller and every Close returns; nothing hangs.
	writers.Wait()
	closers.Wait()
	close(closeErrs)
	for err := range closeErrs {
		if err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	}
	select {
	case <-db.writerDone:
	default:
		t.Error("the writer goroutine is still running after Close()")
	}

	// Reopening is the only honest way to ask what actually reached the volume.
	mu.Lock()
	acknowledged := append([]string(nil), committed...)
	mu.Unlock()

	// Without this the assertions below would hold vacuously for a shutdown
	// that simply won every race.
	if len(acknowledged) < callers {
		t.Fatalf("acknowledged events = %d, want at least %d: the close did not race live traffic",
			len(acknowledged), callers)
	}

	reopened := open(t, dir)
	for _, id := range acknowledged {
		if got := query[int](t, reopened,
			"SELECT count(*) FROM page_view_events WHERE event_id = ?", id); got != 1 {
			t.Fatalf("an acknowledged event is not in the ledger")
		}
	}
	ledger := query[int](t, reopened, "SELECT count(*) FROM page_view_events")
	if ledger != len(acknowledged) {
		t.Errorf("ledger rows = %d, want the %d acknowledged events", ledger, len(acknowledged))
	}
	if got := query[int](t, reopened, "SELECT coalesce(sum(page_views), 0) FROM daily_site_stats"); got != ledger {
		t.Errorf("site page views = %d, want %d", got, ledger)
	}
}

// TestShutdownResolvesQueuedCallers is the shutdown contract: the in-flight
// transaction finishes, everything still queued is answered rather than left
// waiting, nothing uncommitted is acknowledged, and the writer goroutine is
// gone by the time Close returns.
func TestShutdownResolvesQueuedCallers(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "data")
	db := open(t, dir)
	addSite(t, db, testSiteID)

	inFlight := samplePageView()
	blocked := blockWriter(t, db, inFlight)

	const queued = 8
	answers := make(chan error, queued)
	var wg sync.WaitGroup
	for i := range queued {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view := samplePageView()
			view.EventID = eventID(i + 2)
			_, err := db.RecordPageView(context.Background(), view)
			answers <- err
		}()
	}
	waitForQueueDepth(t, db, queued)

	closed := make(chan error, 1)
	go func() { closed <- db.Close() }()

	// Close must wait for the transaction that is already running.
	select {
	case err := <-closed:
		t.Fatalf("Close() returned while a transaction was in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	blocked.release()
	if err := <-closed; err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := <-blocked.finished; err != nil {
		t.Fatalf("the in-flight page view failed: %v", err)
	}

	// Every queued caller was answered, and none of them was acknowledged.
	wg.Wait()
	for range queued {
		err := <-answers
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("queued caller error = %v, want %v", err, ErrClosed)
		}
		assertPrivate(t, err)
	}

	// The writer goroutine has finished, so nothing is left running.
	select {
	case <-db.writerDone:
	default:
		t.Error("the writer goroutine is still running after Close()")
	}

	// And a page view offered after shutdown is refused rather than queued.
	after := samplePageView()
	after.EventID = eventID(100)
	if _, err := db.RecordPageView(context.Background(), after); !errors.Is(err, ErrClosed) {
		t.Errorf("RecordPageView() after Close() error = %v, want %v", err, ErrClosed)
	}

	// Reopening proves what actually reached the volume: the one transaction
	// that had already started, and nothing that was only queued.
	reopened := open(t, dir)
	if got := query[int](t, reopened, "SELECT count(*) FROM page_view_events"); got != 1 {
		t.Errorf("ledger rows = %d, want only the committed in-flight event", got)
	}
	if got := query[int](t, reopened, "SELECT page_views FROM daily_site_stats"); got != 1 {
		t.Errorf("site page views = %d, want 1", got)
	}
}

// TestWritesNeedAUsableDatabase covers the two states in which the store has no
// business writing: before the migrations have run, and after a failure it
// refuses to serve.
func TestWritesNeedAUsableDatabase(t *testing.T) {
	t.Parallel()

	t.Run("not initialized", func(t *testing.T) {
		t.Parallel()

		db := openOnly(t, filepath.Join(t.TempDir(), "data"))
		_, err := db.RecordPageView(context.Background(), samplePageView())
		if !errors.Is(err, ErrUninitialized) {
			t.Fatalf("RecordPageView() error = %v, want %v", err, ErrUninitialized)
		}
		assertPrivate(t, err)
		if db.queueDepth() != 0 {
			t.Error("a page view was queued against a database that is not initialized")
		}
	})

	t.Run("initialization failed", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "data")
		first := open(t, dir)
		exec(t, first, "UPDATE schema_migrations SET checksum = "+
			"'0000000000000000000000000000000000000000000000000000000000000000' WHERE version = 1")
		if err := first.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}

		second := openOnly(t, dir)
		if err := second.Initialize(context.Background()); !errors.Is(err, ErrChecksum) {
			t.Fatalf("Initialize() error = %v, want %v", err, ErrChecksum)
		}
		_, err := second.RecordPageView(context.Background(), samplePageView())
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("RecordPageView() error = %v, want %v", err, ErrChecksum)
		}
		assertPrivate(t, err)
		if got := query[int](t, second, "SELECT count(*) FROM page_view_events"); got != 0 {
			t.Errorf("ledger rows = %d, want 0", got)
		}
	})

	// The remembered startup failure is written for an operator and can quote
	// SQLite — "file is not a database (26)". Readiness keeps that; a caller
	// offering a page view gets the class and nothing else.
	t.Run("the remembered reason is reduced to its class", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "data")
		if err := os.MkdirAll(dir, dirMode); err != nil {
			t.Fatalf("create data directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, DatabaseFileName), []byte("not a database at all"), fileMode); err != nil {
			t.Fatalf("write fixture: %v", err)
		}

		db := openOnly(t, dir)
		initErr := db.Initialize(context.Background())
		if !errors.Is(initErr, ErrIntegrity) {
			t.Fatalf("Initialize() error = %v, want %v", initErr, ErrIntegrity)
		}
		// Readiness still carries the detail an operator needs.
		if !strings.Contains(initErr.Error(), "integrity check") {
			t.Errorf("Initialize() error = %v, want the stage that produced it", initErr)
		}

		_, err := db.RecordPageView(context.Background(), samplePageView())
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("RecordPageView() error = %v, want %v", err, ErrIntegrity)
		}
		if err.Error() != ErrIntegrity.Error() {
			t.Errorf("RecordPageView() error = %q, want the bare class %q", err, ErrIntegrity)
		}
		assertPrivate(t, err)
	})
}
