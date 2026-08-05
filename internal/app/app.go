// Package app assembles the service from its parts.
//
// It exists so that the full wiring — configuration, health registry, router,
// server — can be exercised by tests exactly as the binary runs it, leaving
// main responsible only for flags, process I/O, and exit codes.
package app

import (
	"context"
	"log/slog"
	"net"

	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/health"
	"github.com/mtrenker/pacapulse/internal/httpapi"
	"github.com/mtrenker/pacapulse/internal/server"
)

// App is a configured, not yet running, service instance.
type App struct {
	logger *slog.Logger
	health *health.Registry
	server *server.Server
}

// New assembles the service. It does not bind a port.
func New(cfg config.Config, logger *slog.Logger) *App {
	registry := health.NewRegistry()

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

	return &App{logger: logger, health: registry, server: srv}
}

// Health exposes the readiness registry. Later issues register their
// dependency checks here during wiring.
func (a *App) Health() *health.Registry { return a.health }

// Addr returns the bound listen address, or nil before the listener is open.
func (a *App) Addr() net.Addr { return a.server.Addr() }

// Started is closed once the service is accepting connections.
func (a *App) Started() <-chan struct{} { return a.server.Started() }

// Run serves until ctx is cancelled, then shuts down gracefully.
func (a *App) Run(ctx context.Context) error { return a.server.Run(ctx) }
