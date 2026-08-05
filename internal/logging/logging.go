// Package logging builds the application logger.
//
// PacaPulse logs operational events about the process, never the content of a
// request. Request headers, query strings, request bodies, client addresses,
// cookies, credentials, and environment contents must not be passed to a
// logger anywhere in this repository — see docs/development.md. Keeping that
// rule at the call sites, rather than filtering it out afterwards, is what
// makes it reviewable.
package logging

import (
	"io"
	"log/slog"

	"github.com/mtrenker/pacapulse/internal/config"
)

// New returns the application logger. format is one of config.FormatJSON or
// config.FormatText; any other value falls back to JSON so that a logger always
// exists.
func New(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if format == config.FormatText {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}
	return slog.New(handler)
}
