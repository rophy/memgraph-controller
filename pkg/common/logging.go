package common

import (
	"context"
	"log/slog"
	"os"
	"sync"
)

// Logger wraps slog.Logger with memgraph-controller specific functionality
type Logger struct {
	*slog.Logger
	component string
}

// LoggingConfig holds logger configuration
type LoggingConfig struct {
	Level     string
	Component string
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

	var handler slog.Handler
	if config.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		// Default to text format
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)

	// Add component context if provided
	if config.Component != "" {
		logger = logger.With("component", config.Component)
	}

	return &Logger{
		Logger:    logger,
		component: config.Component,
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

// WithContext creates a logger with context values
func (l *Logger) WithContext(ctx context.Context) *Logger {
	attrs := extractContextAttrs(ctx)
	if len(attrs) == 0 {
		return l
	}

	return &Logger{
		Logger:    l.Logger.With(attrs...),
		component: l.component,
	}
}

// WithFields creates a logger with additional structured fields
func (l *Logger) WithFields(fields map[string]any) *Logger {
	if len(fields) == 0 {
		return l
	}

	args := make([]any, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}

	return &Logger{
		Logger:    l.Logger.With(args...),
		component: l.component,
	}
}

// extractContextAttrs extracts logging attributes from context
func extractContextAttrs(ctx context.Context) []any {
	var attrs []any

	if reqID := ctx.Value("request_id"); reqID != nil {
		attrs = append(attrs, "request_id", reqID)
	}

	if traceID := ctx.Value("trace_id"); traceID != nil {
		attrs = append(attrs, "trace_id", traceID)
	}

	if operation := ctx.Value("operation"); operation != nil {
		attrs = append(attrs, "operation", operation)
	}

	if pod := ctx.Value("pod"); pod != nil {
		attrs = append(attrs, "pod", pod)
	}

	return attrs
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
	})
}

// GetLogger returns the global logger
func GetLogger() *Logger {
	if globalLogger == nil {
		InitLogger()
	}
	return globalLogger
}