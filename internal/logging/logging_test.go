package logging_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/mtrenker/pacapulse/internal/config"
	"github.com/mtrenker/pacapulse/internal/logging"
)

func TestLoggerFormatsAreSelectable(t *testing.T) {
	t.Parallel()

	var jsonLogs, textLogs bytes.Buffer
	logging.New(&jsonLogs, config.FormatJSON, slog.LevelInfo).Info("hello", "key", "value")
	logging.New(&textLogs, config.FormatText, slog.LevelInfo).Info("hello", "key", "value")

	if !strings.HasPrefix(jsonLogs.String(), "{") {
		t.Errorf("json format produced %q", jsonLogs.String())
	}
	if !strings.Contains(textLogs.String(), "key=value") {
		t.Errorf("text format produced %q", textLogs.String())
	}
}

func TestLoggerRespectsLevel(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := logging.New(&logs, config.FormatJSON, slog.LevelWarn)
	logger.Info("suppressed")
	logger.Warn("emitted")

	if strings.Contains(logs.String(), "suppressed") {
		t.Errorf("info record was emitted at warn level: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "emitted") {
		t.Errorf("warn record was not emitted: %s", logs.String())
	}
}
