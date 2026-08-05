package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestObserveKeepsResponseControllerWorking guards against the usual cost of
// wrapping a ResponseWriter: losing the optional interfaces underneath it. The
// probe handlers do not stream, but future collector and dashboard handlers
// will, and the loss would only show up at runtime.
func TestObserveKeepsResponseControllerWorking(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler := observe(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		if _, err := io.WriteString(w, "chunk"); err != nil {
			t.Errorf("write: %v", err)
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush() error = %v, want nil", err)
		}
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if !recorder.Flushed {
		t.Error("the response was not flushed through the wrapper")
	}
	if recorder.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
}
