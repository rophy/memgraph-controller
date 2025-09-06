package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// Logger wraps slog.Logger with memgraph-controller specific functionality
type Logger struct {
	*slog.Logger
	component string
}

// Config holds logger configuration
type Config struct {
	Level     string
	Component string
}

// NewLogger creates a new structured logger with JSONL output
func NewLogger(config Config) *Logger {
	level := parseLevel(config.Level)

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
	}

	handler := slog.NewTextHandler(os.Stdout, opts)
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
	switch strings.ToLower(level) {
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

// NewControllerLogger creates a logger for controller components
func NewControllerLogger(level string) *Logger {
	return NewLogger(Config{
		Level:     level,
		Component: "controller",
	})
}

// NewGatewayLogger creates a logger for gateway components
func NewGatewayLogger(level string) *Logger {
	return NewLogger(Config{
		Level:     level,
		Component: "gateway",
	})
}

// GetLogLevel returns log level from environment or default
func GetLogLevel() string {
	if level := os.Getenv("LOG_LEVEL"); level != "" {
		return level
	}
	return "info"
}
