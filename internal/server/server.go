// Package server runs the HTTP listener with bounded timeouts and a graceful
// shutdown sequence.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Options configures a Server. Addr, Handler, and Logger are required; the
// timeouts are supplied by the caller so that this package does not depend on
// the configuration package.
type Options struct {
	Addr    string
	Handler http.Handler
	Logger  *slog.Logger

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	MaxHeaderBytes    int

	// BeforeShutdown runs once termination begins, before in-flight requests
	// are drained. It is where readiness is flipped so that traffic stops
	// arriving while existing work finishes.
	BeforeShutdown func()
}

// Server owns the listener and the shutdown sequence.
type Server struct {
	http            *http.Server
	logger          *slog.Logger
	listenAddr      string
	shutdownTimeout time.Duration
	beforeShutdown  func()

	started chan struct{}
	mu      sync.Mutex
	addr    net.Addr
}

// New builds a server from opts. It does not bind; call Run.
func New(opts Options) *Server {
	return &Server{
		http: &http.Server{
			Handler:           opts.Handler,
			ReadHeaderTimeout: opts.ReadHeaderTimeout,
			ReadTimeout:       opts.ReadTimeout,
			WriteTimeout:      opts.WriteTimeout,
			IdleTimeout:       opts.IdleTimeout,
			MaxHeaderBytes:    opts.MaxHeaderBytes,
			// net/http's own error log reports transport-level faults, and
			// some of those messages embed the peer address. It is routed at
			// debug level so it stays out of the default log while remaining
			// available when an operator deliberately raises the level.
			ErrorLog: slog.NewLogLogger(opts.Logger.Handler(), slog.LevelDebug),
		},
		logger:          opts.Logger,
		listenAddr:      opts.Addr,
		shutdownTimeout: opts.ShutdownTimeout,
		beforeShutdown:  opts.BeforeShutdown,
		started:         make(chan struct{}),
	}
}

// Started is closed once the listener is bound and Addr is populated. It stays
// open if binding fails, so a caller waiting on it should also watch the result
// of Run.
func (s *Server) Started() <-chan struct{} { return s.started }

// Addr returns the bound address, or nil before the listener is open. It is
// what tests and local runs use to resolve an ephemeral port.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Run binds the listener and serves until ctx is cancelled or the server fails.
// It must be called at most once per Server.
//
// On cancellation it stops accepting connections, lets in-flight requests
// finish within ShutdownTimeout, and returns nil. If the grace period expires
// first, remaining connections are closed and the deadline error is returned,
// so an operator sees that termination was not clean.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		// The listen address is operator-supplied infrastructure detail, not a
		// secret, and the error is useless without it.
		return fmt.Errorf("listen on %s: %w", s.listenAddr, err)
	}

	s.mu.Lock()
	s.addr = listener.Addr()
	s.mu.Unlock()
	close(s.started)

	s.logger.InfoContext(ctx, "http server listening", "addr", listener.Addr().String())

	serveErr := make(chan error, 1)
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		// Serve returned without Shutdown being called; the listener is gone.
		return errors.New("serve: listener closed unexpectedly")
	case <-ctx.Done():
	}

	if s.beforeShutdown != nil {
		s.beforeShutdown()
	}
	s.logger.Info("http server draining", "grace_period", s.shutdownTimeout.String())

	// The parent context is already cancelled, so the shutdown deadline is
	// derived from a fresh context rather than inheriting the cancellation.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()

	if err := s.http.Shutdown(shutdownCtx); err != nil {
		_ = s.http.Close()
		<-serveErr
		return fmt.Errorf("graceful shutdown exceeded %s: %w", s.shutdownTimeout, err)
	}

	if err := <-serveErr; err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	s.logger.Info("http server stopped")
	return nil
}
