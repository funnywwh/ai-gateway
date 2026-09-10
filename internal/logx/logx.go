// Package logx provides the gateway's structured logger.
package logx

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config controls logger construction.
type Config struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // text|json
}

// Default returns the default logging configuration.
func Default() Config { return Config{Level: "info", Format: "text"} }

// ParseLevel maps a level string to a slog.Level (unknown values => info).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
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

// New builds a slog.Logger from cfg.
func New(cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: ParseLevel(cfg.Level)}
	var h slog.Handler
	if strings.EqualFold(strings.TrimSpace(cfg.Format), "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// Nop returns a logger that discards all records (used by tests).
func Nop() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
