// Package health tracks process liveness and readiness.
//
// Liveness and readiness are deliberately distinct. Liveness answers "is this
// process still functioning?" and is true for as long as the HTTP server can
// serve a request. Readiness answers "should this instance receive traffic?"
// and is the extension point where later issues register dependency checks
// (storage, OIDC discovery) without touching the server or router.
package health

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Check reports whether a dependency is usable. It must return promptly and
// respect the context deadline; a nil error means ready.
type Check func(ctx context.Context) error

// Failure describes one failing readiness check.
type Failure struct {
	// Name identifies the check.
	Name string
	// Err is the underlying error. It is for logs only: dependency errors can
	// carry connection strings or hostnames, so it is never rendered into an
	// HTTP response.
	Err error
}

// Status is the outcome of evaluating readiness.
type Status struct {
	// Ready is true when the process is not draining and every check passed.
	Ready bool
	// Draining is true after BeginDrain, i.e. once termination has started.
	Draining bool
	// Failed lists the failing checks, sorted by name.
	Failed []Failure
}

type registered struct {
	name  string
	check Check
}

// Registry holds the readiness checks of the process. The zero value is not
// usable; call NewRegistry. A Registry is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	checks   []registered
	draining atomic.Bool
}

// NewRegistry returns an empty registry, which reports ready.
func NewRegistry() *Registry { return &Registry{} }

// Register adds a readiness check. Registering the same name twice replaces the
// previous check, which keeps wiring order from silently duplicating work.
func (r *Registry) Register(name string, check Check) {
	if check == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.checks {
		if r.checks[i].name == name {
			r.checks[i].check = check
			return
		}
	}
	r.checks = append(r.checks, registered{name: name, check: check})
}

// BeginDrain marks the process as no longer ready for new traffic. It is called
// when a termination signal arrives, before in-flight requests are drained, so
// that a load balancer or Kubernetes endpoint controller removes this instance
// while it finishes existing work.
func (r *Registry) BeginDrain() { r.draining.Store(true) }

// Draining reports whether termination has started.
func (r *Registry) Draining() bool { return r.draining.Load() }

// Readiness evaluates every registered check. Checks run sequentially: the
// baseline has none, and a small dependency set does not justify the
// concurrency.
func (r *Registry) Readiness(ctx context.Context) Status {
	if r.draining.Load() {
		return Status{Draining: true}
	}

	r.mu.RLock()
	checks := make([]registered, len(r.checks))
	copy(checks, r.checks)
	r.mu.RUnlock()

	var failed []Failure
	for _, c := range checks {
		if err := c.check(ctx); err != nil {
			failed = append(failed, Failure{Name: c.name, Err: err})
		}
	}
	slices.SortFunc(failed, func(a, b Failure) int { return strings.Compare(a.Name, b.Name) })

	return Status{Ready: len(failed) == 0, Failed: failed}
}
