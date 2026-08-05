package server_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mtrenker/pacapulse/internal/server"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// start runs a server on an ephemeral loopback port and returns its base URL
// plus a channel carrying the eventual result of Run.
func start(t *testing.T, opts server.Options) (string, <-chan error) {
	t.Helper()

	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	opts.ReadHeaderTimeout = time.Second
	opts.ReadTimeout = 5 * time.Second
	opts.WriteTimeout = 10 * time.Second
	opts.IdleTimeout = 10 * time.Second
	opts.MaxHeaderBytes = 64 << 10

	srv := server.New(opts)
	result := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { result <- srv.Run(ctx) }()

	select {
	case <-srv.Started():
	case err := <-result:
		t.Fatalf("Run() returned before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start within 5s")
	}

	return "http://" + srv.Addr().String(), result
}

func TestServerServesUntilContextIsCancelled(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(server.Options{
		Addr: "127.0.0.1:0", Handler: handler, Logger: discardLogger(),
		ShutdownTimeout: 5 * time.Second,
	})
	result := make(chan error, 1)
	go func() { result <- srv.Run(ctx) }()
	<-srv.Started()

	addr := srv.Addr().String()

	// A keep-alive connection is held open across the shutdown. Asserting that
	// the server closes it is more reliable than re-dialling the port, which a
	// concurrently starting test could have taken over by then.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial before shutdown: %v", err)
	}
	defer func() { _ = conn.Close() }()

	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after cancellation")
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("reading the idle connection after shutdown returned %v, want %v", err, io.EOF)
	}
}

// TestShutdownDrainsInFlightRequests covers the termination contract: work
// already accepted finishes, and Run reports success.
func TestShutdownDrainsInFlightRequests(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "drained")
	})

	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(server.Options{
		Addr: "127.0.0.1:0", Handler: handler, Logger: discardLogger(),
		ShutdownTimeout: 10 * time.Second,
	})
	result := make(chan error, 1)
	go func() { result <- srv.Run(ctx) }()
	<-srv.Started()
	base := "http://" + srv.Addr().String()

	type response struct {
		body string
		err  error
	}
	responses := make(chan response, 1)
	go func() {
		resp, err := http.Get(base + "/slow")
		if err != nil {
			responses <- response{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		responses <- response{body: string(body), err: err}
	}()

	<-entered
	cancel()
	// Give shutdown a moment to start while the request is still in flight.
	time.Sleep(50 * time.Millisecond)
	close(release)

	got := <-responses
	if got.err != nil {
		t.Fatalf("in-flight request failed during shutdown: %v", got.err)
	}
	if got.body != "drained" {
		t.Errorf("in-flight response body = %q, want %q", got.body, "drained")
	}

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after draining")
	}
}

// TestShutdownReportsExceededGracePeriod ensures an unclean termination is
// visible rather than silently swallowed.
func TestShutdownReportsExceededGracePeriod(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	})

	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(server.Options{
		Addr: "127.0.0.1:0", Handler: handler, Logger: discardLogger(),
		ShutdownTimeout: 100 * time.Millisecond,
	})
	result := make(chan error, 1)
	go func() { result <- srv.Run(ctx) }()
	<-srv.Started()
	base := "http://" + srv.Addr().String()

	go func() {
		resp, err := http.Get(base + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-entered
	cancel()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Run() error = nil, want a grace-period error")
		}
		if !strings.Contains(err.Error(), "graceful shutdown exceeded") {
			t.Errorf("Run() error = %v, want it to mention the exceeded grace period", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after the grace period expired")
	}
}

// TestBeforeShutdownRunsBeforeDraining pins the ordering readiness depends on:
// traffic must stop being routed here before in-flight work is drained.
func TestBeforeShutdownRunsBeforeDraining(t *testing.T) {
	t.Parallel()

	called := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	srv := server.New(server.Options{
		Addr:            "127.0.0.1:0",
		Handler:         http.NotFoundHandler(),
		Logger:          discardLogger(),
		ShutdownTimeout: 5 * time.Second,
		BeforeShutdown:  func() { close(called) },
	})
	result := make(chan error, 1)
	go func() { result <- srv.Run(ctx) }()
	<-srv.Started()

	cancel()

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("BeforeShutdown was not called")
	}
	if err := <-result; err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
}

func TestRunReportsListenFailure(t *testing.T) {
	t.Parallel()

	base, _ := start(t, server.Options{Handler: http.NotFoundHandler()})
	taken := strings.TrimPrefix(base, "http://")

	srv := server.New(server.Options{
		Addr: taken, Handler: http.NotFoundHandler(), Logger: discardLogger(),
		ShutdownTimeout: time.Second,
	})
	err := srv.Run(context.Background())
	if err == nil {
		t.Fatal("Run() error = nil, want a listen failure")
	}
	if !strings.Contains(err.Error(), "listen on "+taken) {
		t.Errorf("Run() error = %v, want it to name the listen address", err)
	}
}

func TestAddrIsNilBeforeListening(t *testing.T) {
	t.Parallel()

	srv := server.New(server.Options{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler(), Logger: discardLogger()})
	if addr := srv.Addr(); addr != nil {
		t.Errorf("Addr() = %v before Run, want nil", addr)
	}
}
