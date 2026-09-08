package config

import (
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process logger, labelled with the binary that owns it.
//
// All three binaries call this so a line from the worker is shaped like a line
// from the API: the aggregator holds one schema, and a query for failed requests
// does not need a per-binary variant. service is the label ("api", "admin",
// "worker") that tells them apart.
func (c *Config) NewLogger(service string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.logLevel()}

	var handler slog.Handler
	if c.LogFormat == LogFormatText {
		handler = slog.NewTextHandler(os.Stderr, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	}

	return slog.New(handler).With(
		slog.String("service", service),
		slog.String("env", string(c.AppEnv)),
	)
}

// logLevel maps the configured name onto slog's levels. An unrecognised value
// cannot reach here: Load rejects it.
func (c *Config) logLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
