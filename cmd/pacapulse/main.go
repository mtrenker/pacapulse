// Command pacapulse runs the PacaPulse HTTP service.
//
// Configuration comes from the environment; see docs/development.md. The
// process exits 0 after a clean shutdown, 1 on a runtime failure, and 2 on a
// configuration or usage error.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mtrenker/pacapulse/internal/app"
	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/logging"
	"github.com/mtrenker/pacapulse/internal/version"
)

// Process exit codes. Separating configuration failures from runtime failures
// lets an orchestrator distinguish "this will never start" from "this stopped".
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.Stdout, os.Stderr))
}

// report writes a diagnostic to the process error stream. A failed write to
// stderr is not actionable — there is nowhere left to report it — so the error
// is dropped deliberately rather than by omission.
func report(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// run is main with its process dependencies injected so that tests can drive
// the real startup path.
func run(args []string, lookup config.Lookup, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pacapulse", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print the build version and exit")
	healthCheck := flags.Bool("health-check", false, "probe the local readiness endpoint and exit; used as the container health check")
	flags.Usage = func() {
		report(stderr, "Usage: pacapulse [flags]\n\nPacaPulse is configured through the environment; see docs/development.md.\n\nFlags:\n")
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		return exitConfig
	}
	if flags.NArg() > 0 {
		report(stderr, "pacapulse: unexpected argument %q\n", flags.Arg(0))
		return exitConfig
	}

	switch {
	case *showVersion:
		report(stdout, "%s\n", version.Version())
		return exitOK
	case *healthCheck:
		return probeSelf(lookup, stderr)
	}

	cfg, err := config.Load(lookup)
	if err != nil {
		// Written before the logger exists, and deliberately plain: the
		// message names the variables at fault without their values.
		report(stderr, "pacapulse: %v\n", err)
		return exitConfig
	}

	logger := logging.New(stdout, cfg.LogFormat, cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("starting pacapulse",
		"version", version.Version(),
		"shutdown_timeout", cfg.ShutdownTimeout.String(),
	)

	service, err := app.New(ctx, cfg, logger)
	if err != nil {
		// Storage the process cannot own at all: an unusable data directory or
		// a database another process is already writing. The record names the
		// failure, never the configured path.
		logger.Error("pacapulse failed to start", "error", err)
		return exitFailure
	}

	if err := service.Run(ctx); err != nil {
		logger.Error("pacapulse stopped", "error", err)
		return exitFailure
	}
	logger.Info("pacapulse stopped")
	return exitOK
}
