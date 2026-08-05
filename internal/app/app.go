// Package app assembles the service from its parts.
//
// It exists so that the full wiring — configuration, storage, health registry,
// router, server — can be exercised by tests exactly as the binary runs it,
// leaving main responsible only for flags, process I/O, and exit codes.
package app

import (
	"context"
	"log/slog"
	"net"
	"sync"

	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/health"
	"github.com/mtrenker/pacapulse/internal/httpapi"
	"github.com/mtrenker/pacapulse/internal/server"
	"github.com/mtrenker/pacapulse/internal/storage"
)

// StorageCheck is the name the database reports itself under in readiness.
const StorageCheck = "database"

// App is a configured, not yet running, service instance.
type App struct {
	logger  *slog.Logger
	health  *health.Registry
	server  *server.Server
	storage *storage.Database
	close   sync.Once
}

// New assembles the service. It does not bind a port.
//
// It returns an error only for the failures that leave the process nothing to
// run: an unusable data directory, or a database another process already owns.
// A database that cannot be checked or migrated is not one of them — the
// service starts, reports itself unready, and stays available for diagnosis.
func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*App, error) {
	registry := health.NewRegistry()

	store, err := storage.Open(storage.Options{Dir: cfg.DataDir})
	if err != nil {
		return nil, err
	}
	if err := store.Initialize(ctx); err != nil {
		// The record names the failure class and the stage, never the path,
		// the SQL, or any stored value. Readiness reports the check name only.
		logger.Error("database is unavailable", "error", err)
	} else {
		logger.Info("database ready", "schema_version", store.Version())
	}
	registry.Register(StorageCheck, store.Check)

	router := httpapi.NewRouter(httpapi.Options{
		Logger: logger,
		Health: registry,
	})

	srv := server.New(server.Options{
		Addr:              cfg.Addr,
		Handler:           router,
		Logger:            logger,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ShutdownTimeout:   cfg.ShutdownTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		// Stop advertising readiness before draining, so that traffic is
		// routed elsewhere while in-flight requests finish.
		BeforeShutdown: registry.BeginDrain,
	})

	return &App{logger: logger, health: registry, server: srv, storage: store}, nil
}

// Health exposes the readiness registry. Later issues register their
// dependency checks here during wiring.
func (a *App) Health() *health.Registry { return a.health }

// Storage exposes the database so that later issues can reach it during wiring.
func (a *App) Storage() *storage.Database { return a.storage }

// Addr returns the bound listen address, or nil before the listener is open.
func (a *App) Addr() net.Addr { return a.server.Addr() }

// Started is closed once the service is accepting connections.
func (a *App) Started() <-chan struct{} { return a.server.Started() }

// Run serves until ctx is cancelled, then shuts down gracefully. The database
// is closed and its lock released once draining has finished, so the volume is
// free for the next process before this one exits.
func (a *App) Run(ctx context.Context) error {
	err := a.server.Run(ctx)
	if closeErr := a.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	return err
}

// Close releases what New acquired. It is safe to call more than once, so a
// caller that never reaches Run can release the database lock itself.
func (a *App) Close() error {
	var err error
	a.close.Do(func() { err = a.storage.Close() })
	return err
}
