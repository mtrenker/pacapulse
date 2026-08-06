package storage

import (
	"bytes"
	"context"
)

// writeQueueCapacity is how many accepted page views may wait for the writer.
//
// The bound is fixed for the MVP rather than tunable, for the same reason the
// cardinality limits are: it is a guarantee about the process, not a knob. It
// is chosen so that a burst costs bounded memory and bounded latency.
//
//   - Memory: a queued page view is the bounded record the data model sizes at
//     about 1 KiB at its maximum path and campaign lengths, so a full queue
//     pins well under a megabyte — irrelevant beside the 50–150 MiB idle
//     target, and far too small to be an amplification vector.
//   - Latency: one event is one immediate transaction committed with
//     synchronous = FULL, a few milliseconds on the intended volume. A full
//     queue therefore drains in roughly a second, which is inside a collector
//     request's useful lifetime; a deeper queue would only accept work whose
//     caller has already given up.
//
// Beyond that, rejecting is the honest answer. The data model requires
// backpressure rather than a silently dropped acknowledgement, so a caller that
// cannot be served now is told so and may retry. The case that makes this
// visible is a busy writer: one attempt can wait out the 5s busy_timeout, and
// the queue filling behind it is exactly when callers should be turned away
// rather than accumulated.
const writeQueueCapacity = 256

// writeRequest is one page view waiting for the writer goroutine.
type writeRequest struct {
	// ctx is the caller's context. It decides whether the request is still
	// wanted when the writer reaches it; the transaction itself then runs under
	// the writer's context. See serve.
	ctx   context.Context
	view  PageView
	reply chan writeResult
}

// writeResult is what the writer goroutine sends back. The channel is buffered,
// so the writer can never block on a caller that has already stopped listening.
type writeResult struct {
	result PageViewResult
	err    error
}

// failureHook is the test-only injection point for a failing transaction stage.
// It is a pointer swapped atomically, so production pays one atomic load per
// stage and the race detector has nothing to complain about when a test sets it
// while the writer goroutine is running.
type failureHook struct {
	fn func(stage string) error
}

// injectedFailure reports the error a test has asked this stage to fail with.
func (d *Database) injectedFailure(stage string) error {
	hook := d.fail.Load()
	if hook == nil {
		return nil
	}
	return hook.fn(stage)
}

// RecordPageView durably records one already-sanitized page view.
//
// The page view is validated first, then handed to the single writer goroutine,
// which runs one immediate SQLite transaction: the 30-day ledger row, the day's
// site and page totals, the referrer and campaign totals when those dimensions
// are present, and the visitor-deduplication rows when a visitor key is. It
// returns only after that transaction has committed, so a successful return
// means the measurement is durable and a failed one means none of it was kept.
//
// Repeating a (SiteID, EventID) pair is a success whose result reports
// Duplicate: the first accepted delivery stands, and nothing is counted twice.
//
// The errors it returns are all typed and carry no identifier, dimension,
// visitor key, SQL text, or database path:
//
//   - ErrInvalidPageView — the input does not satisfy the ingestion contract.
//   - ErrUnknownSite — the site does not exist, or was deleted while the page
//     view was queued.
//   - ErrQueueFull — the writer is saturated. Retryable.
//   - ErrUnavailable — SQLite could not accept the write now. Retryable.
//   - ErrClosed, ErrUninitialized — the store is not accepting writes.
//   - ErrWrite, ErrIntegrity — the write failed, and nothing was kept.
//   - ctx.Err() — the caller gave up. If it did so while queued, the page view
//     is abandoned without being executed.
func (d *Database) RecordPageView(ctx context.Context, view PageView) (PageViewResult, error) {
	// Validation happens on the caller's goroutine and before the queue: a
	// malformed page view must not consume writer capacity, and rejecting it
	// early is what guarantees it leaves nothing behind to undo.
	if err := validatePageView(view); err != nil {
		return PageViewResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return PageViewResult{}, err
	}

	d.mu.Lock()
	closed, initErr := d.closed, d.initErr
	d.mu.Unlock()
	switch {
	case closed:
		return PageViewResult{}, ErrClosed
	case initErr != nil:
		// A database that failed its checks or migrations is not written to.
		// The caller gets the class alone; the remembered detail stays in
		// readiness and the startup log, where an operator reads it.
		return PageViewResult{}, ingestionClass(initErr)
	}

	// The visitor key is copied before it is queued. It is the caller's slice,
	// and a caller that stops waiting — a timed-out collection request — may
	// reuse or clear that buffer while the writer is still using it. Sharing it
	// would be both a data race and a way for one visitor's deduplication
	// identity to become another's.
	view.VisitorKey = bytes.Clone(view.VisitorKey)

	request := &writeRequest{ctx: ctx, view: view, reply: make(chan writeResult, 1)}
	select {
	case d.queue <- request:
	case <-d.stop:
		return PageViewResult{}, ErrClosed
	case <-ctx.Done():
		return PageViewResult{}, ctx.Err()
	default:
		// Full. Rejecting immediately is the documented contract: an unbounded
		// wait would hold a collector request open past its usefulness, and
		// dropping the page view after acknowledging it would be a lie.
		return PageViewResult{}, describe(ErrQueueFull, "accept page view",
			"the write queue is full; retry later")
	}

	select {
	case result := <-request.reply:
		return result.result, result.err
	case <-ctx.Done():
		// The writer still owns the request. Its reply channel is buffered, so
		// the send it may already be making cannot block, and this goroutine
		// leaves nothing behind.
		return PageViewResult{}, ctx.Err()
	case <-d.writerDone:
		// The writer exited. It either replied on its way out or never reached
		// this request; both are answered here rather than waited on forever.
		select {
		case result := <-request.reply:
			return result.result, result.err
		default:
			return PageViewResult{}, ErrClosed
		}
	}
}

// runWriter is the single writer goroutine. Every write the process makes goes
// through it, so SQLite's one writer is enforced inside the process as well as
// between processes by the advisory lock.
func (d *Database) runWriter() {
	defer close(d.writerDone)

	for {
		// Shutdown is checked first so that a stop the writer has already been
		// told about wins over further queued work, instead of being decided by
		// a random select.
		select {
		case <-d.stop:
			d.abandonQueued()
			return
		default:
		}

		select {
		case <-d.stop:
			d.abandonQueued()
			return
		case request := <-d.queue:
			d.serve(request)
		}
	}
}

// serve runs one request's transaction and answers its caller exactly once.
func (d *Database) serve(request *writeRequest) {
	// A caller that gave up while queued is abandoned without being executed:
	// its transaction would only be rolled back, and running it would spend the
	// writer's time on work nobody is waiting for.
	if err := request.ctx.Err(); err != nil {
		request.reply <- writeResult{err: err}
		return
	}
	// The transaction then runs under the writer's own context rather than the
	// caller's. Cancelling a statement mid-transaction interrupts SQLite and
	// costs the pool its single connection, and an event that is started is
	// cheaper to finish than to abandon halfway; a caller that stops waiting
	// simply stops waiting. It is also what lets shutdown promise that the
	// in-flight transaction reaches a commit or a rollback.
	result, err := d.recordPageView(context.Background(), request.view)
	request.reply <- writeResult{result: result, err: err}
}

// abandonQueued resolves every request still waiting when shutdown began.
//
// None of them was executed, so none may be acknowledged. Answering them is
// what keeps their callers from waiting on a writer that has stopped; a request
// that races past this drain is answered by RecordPageView's writerDone case.
func (d *Database) abandonQueued() {
	for {
		select {
		case request := <-d.queue:
			request.reply <- writeResult{err: ErrClosed}
		default:
			return
		}
	}
}

// queueDepth reports how many requests are waiting. It exists for the tests
// that have to observe a saturated queue deterministically.
func (d *Database) queueDepth() int { return len(d.queue) }
