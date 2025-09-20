package common

import (
	"context"
	"log/slog"
	"os"
	"sync"

	slogctx "github.com/veqryn/slog-context"
)

// Logger wraps slog.Logger with memgraph-controller specific functionality
type Logger struct {
	*slog.Logger
}

// LoggingConfig holds logger configuration
type LoggingConfig struct {
	Level     string
	AddSource bool
	Format    string
}

var (
	globalLogger *Logger
	once         sync.Once
)

// NewLogger creates a new structured logger with configurable output
func NewLogger(config LoggingConfig) *Logger {
	level := parseLevel(config.Level)

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: config.AddSource,
	}

	var baseHandler slog.Handler
	if config.Format == "json" {
		baseHandler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		// Default to text format
		baseHandler = slog.NewTextHandler(os.Stdout, opts)
	}
	handler := slogctx.NewHandler(baseHandler, &slogctx.HandlerOptions{})
	logger := slog.New(handler)

	return &Logger{
		Logger: logger,
	}
}

// parseLevel converts string level to slog.Level
func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// InitLogger initializes the global logger using the centralized config
func InitLogger() {
	once.Do(func() {
		config, _ := Load()
		globalLogger = NewLogger(LoggingConfig{
			Level:     config.LogLevel,
			AddSource: config.LogAddSource,
			Format:    config.LogFormat,
		})
		slog.SetDefault(globalLogger.Logger)
	})
}

// GetLogger returns the global logger
func GetLogger() *slog.Logger {
	if globalLogger == nil {
		InitLogger()
	}
	return globalLogger.Logger
}

// NewLoggerContext returns a new context and the attached logger
func NewLoggerContext(ctx context.Context) (context.Context, *slog.Logger) {
	newCtx := slogctx.NewCtx(ctx, GetLogger())
	return newCtx, slogctx.FromCtx(newCtx)
}

// GetLoggerFromContext returns the logger from the context
func GetLoggerFromContext(ctx context.Context) *slog.Logger {
	return slogctx.FromCtx(ctx)
}

// WithAttr add attributes to the context
func WithAttr(ctx context.Context, args ...any) (context.Context, *slog.Logger) {
	newCtx := slogctx.With(ctx, args...)
	return newCtx, slogctx.FromCtx(newCtx)
}
