package main

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/httpapi"
)

// probeTimeout bounds the container health check. It is short: the check runs
// against loopback, and a slow answer is itself a failure signal.
const probeTimeout = 2 * time.Second

// probeSelf implements `pacapulse -health-check`.
//
// The runtime image has no shell and no curl, so the health check is the
// service binary probing its own readiness endpoint over loopback. Exit 0 means
// healthy and exit 1 means unhealthy, which is the contract both the OCI
// HEALTHCHECK instruction and the local smoke test rely on.
func probeSelf(lookup config.Lookup, stderr io.Writer) int {
	target, err := config.ProbeURL(lookup, httpapi.ReadinessPath)
	if err != nil {
		report(stderr, "pacapulse: %v\n", err)
		return exitConfig
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		report(stderr, "pacapulse: build health check request: %v\n", err)
		return exitFailure
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		report(stderr, "pacapulse: health check request failed\n")
		return exitFailure
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		report(stderr, "pacapulse: health check returned status %d\n", resp.StatusCode)
		return exitFailure
	}
	return exitOK
}
