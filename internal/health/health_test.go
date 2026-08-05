package health_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mtrenker/pacapulse/internal/health"
)

func TestEmptyRegistryIsReady(t *testing.T) {
	t.Parallel()

	status := health.NewRegistry().Readiness(t.Context())
	if !status.Ready {
		t.Errorf("Ready = false, want true for a registry without checks")
	}
	if status.Draining {
		t.Errorf("Draining = true, want false")
	}
	if len(status.Failed) != 0 {
		t.Errorf("Failed = %v, want none", status.Failed)
	}
}

func TestFailingCheckMakesRegistryUnready(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("dial tcp 10.9.8.7:5432: connection refused")
	registry := health.NewRegistry()
	registry.Register("storage", func(context.Context) error { return wantErr })
	registry.Register("identity", func(context.Context) error { return nil })

	status := registry.Readiness(t.Context())
	if status.Ready {
		t.Fatal("Ready = true, want false while a check is failing")
	}
	if len(status.Failed) != 1 {
		t.Fatalf("Failed = %v, want exactly one entry", status.Failed)
	}
	if status.Failed[0].Name != "storage" {
		t.Errorf("Failed[0].Name = %q, want %q", status.Failed[0].Name, "storage")
	}
	if !errors.Is(status.Failed[0].Err, wantErr) {
		t.Errorf("Failed[0].Err = %v, want %v", status.Failed[0].Err, wantErr)
	}
}

func TestFailuresAreSortedByName(t *testing.T) {
	t.Parallel()

	registry := health.NewRegistry()
	for _, name := range []string{"storage", "identity", "beacon"} {
		registry.Register(name, func(context.Context) error { return errors.New("down") })
	}

	status := registry.Readiness(t.Context())
	got := make([]string, 0, len(status.Failed))
	for _, failure := range status.Failed {
		got = append(got, failure.Name)
	}
	want := []string{"beacon", "identity", "storage"}
	if len(got) != len(want) {
		t.Fatalf("Failed names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Failed names = %v, want %v", got, want)
		}
	}
}

func TestBeginDrainOverridesPassingChecks(t *testing.T) {
	t.Parallel()

	registry := health.NewRegistry()
	registry.Register("storage", func(context.Context) error { return nil })

	registry.BeginDrain()

	if !registry.Draining() {
		t.Error("Draining() = false after BeginDrain")
	}
	status := registry.Readiness(t.Context())
	if status.Ready {
		t.Error("Ready = true while draining, want false")
	}
	if !status.Draining {
		t.Error("Draining = false in status, want true")
	}
}

func TestRegisterReplacesSameName(t *testing.T) {
	t.Parallel()

	registry := health.NewRegistry()
	registry.Register("storage", func(context.Context) error { return errors.New("down") })
	registry.Register("storage", func(context.Context) error { return nil })

	if status := registry.Readiness(t.Context()); !status.Ready {
		t.Errorf("Ready = false, want true after the check was replaced (failed: %v)", status.Failed)
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	registry := health.NewRegistry()
	registry.Register("storage", func(context.Context) error { return nil })

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			registry.Register("check", func(context.Context) error { return nil })
			registry.Readiness(context.Background())
			if i == 0 {
				registry.BeginDrain()
			}
		}()
	}
	wg.Wait()
}
