package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"time"
)

// LogLevel represents different log levels
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

// Logger provides structured logging for the gateway
type Logger struct {
	level       LogLevel
	traceEnabled bool
}

// LogEntry represents a structured log entry
type LogEntry struct {
	Timestamp time.Time              `json:"timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Component string                 `json:"component"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
	TraceID   string                 `json:"trace_id,omitempty"`
}

// NewLogger creates a new structured logger
func NewLogger(level string, traceEnabled bool) *Logger {
	return &Logger{
		level:       parseLogLevel(level),
		traceEnabled: traceEnabled,
	}
}

// parseLogLevel converts string to LogLevel
func parseLogLevel(level string) LogLevel {
	switch strings.ToLower(level) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "warn":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

// Debug logs a debug message
func (l *Logger) Debug(message string, fields ...map[string]interface{}) {
	if l.level <= LogLevelDebug {
		l.log(LogLevelDebug, message, fields...)
	}
}

// Info logs an info message
func (l *Logger) Info(message string, fields ...map[string]interface{}) {
	if l.level <= LogLevelInfo {
		l.log(LogLevelInfo, message, fields...)
	}
}

// Warn logs a warning message
func (l *Logger) Warn(message string, fields ...map[string]interface{}) {
	if l.level <= LogLevelWarn {
		l.log(LogLevelWarn, message, fields...)
	}
}

// Error logs an error message
func (l *Logger) Error(message string, fields ...map[string]interface{}) {
	if l.level <= LogLevelError {
		l.log(LogLevelError, message, fields...)
	}
}

// log writes a structured log entry
func (l *Logger) log(level LogLevel, message string, fields ...map[string]interface{}) {
	entry := LogEntry{
		Timestamp: time.Now().UTC(),
		Level:     levelString(level),
		Message:   message,
		Component: "gateway",
	}
	
	// Merge all fields
	if len(fields) > 0 {
		entry.Fields = make(map[string]interface{})
		for _, fieldMap := range fields {
			for k, v := range fieldMap {
				entry.Fields[k] = v
			}
		}
	}
	
	// Add trace ID if tracing is enabled
	if l.traceEnabled {
		entry.TraceID = generateTraceID()
	}
	
	// Output as JSON for structured logging
	if data, err := json.Marshal(entry); err == nil {
		log.Println(string(data))
	} else {
		// Fallback to simple logging if JSON marshaling fails
		log.Printf("[%s] Gateway: %s", strings.ToUpper(entry.Level), message)
	}
}

// levelString converts LogLevel to string
func levelString(level LogLevel) string {
	switch level {
	case LogLevelDebug:
		return "debug"
	case LogLevelInfo:
		return "info"
	case LogLevelWarn:
		return "warn"
	case LogLevelError:
		return "error"
	default:
		return "info"
	}
}

// generateTraceID generates a simple trace ID for distributed tracing
func generateTraceID() string {
	return fmt.Sprintf("gw-%d-%d", time.Now().UnixNano(), os.Getpid())
}

// WithFields creates a logger context with predefined fields
func (l *Logger) WithFields(fields map[string]interface{}) *LoggerContext {
	return &LoggerContext{
		logger: l,
		fields: fields,
	}
}

// LoggerContext provides a logger with predefined fields
type LoggerContext struct {
	logger *Logger
	fields map[string]interface{}
}

// Debug logs a debug message with context fields
func (lc *LoggerContext) Debug(message string, additionalFields ...map[string]interface{}) {
	fields := []map[string]interface{}{lc.fields}
	fields = append(fields, additionalFields...)
	lc.logger.Debug(message, fields...)
}

// Info logs an info message with context fields
func (lc *LoggerContext) Info(message string, additionalFields ...map[string]interface{}) {
	fields := []map[string]interface{}{lc.fields}
	fields = append(fields, additionalFields...)
	lc.logger.Info(message, fields...)
}

// Warn logs a warning message with context fields
func (lc *LoggerContext) Warn(message string, additionalFields ...map[string]interface{}) {
	fields := []map[string]interface{}{lc.fields}
	fields = append(fields, additionalFields...)
	lc.logger.Warn(message, fields...)
}

// Error logs an error message with context fields
func (lc *LoggerContext) Error(message string, additionalFields ...map[string]interface{}) {
	fields := []map[string]interface{}{lc.fields}
	fields = append(fields, additionalFields...)
	lc.logger.Error(message, fields...)
}

// LogConnectionEvent logs a connection-related event with standard fields
func (l *Logger) LogConnectionEvent(event string, clientAddr string, fields ...map[string]interface{}) {
	_, file, line, _ := runtime.Caller(1)
	
	baseFields := map[string]interface{}{
		"event":       event,
		"client_addr": clientAddr,
		"file":        file,
		"line":        line,
	}
	
	allFields := []map[string]interface{}{baseFields}
	allFields = append(allFields, fields...)
	
	l.Info(fmt.Sprintf("Connection %s: %s", event, clientAddr), allFields...)
}

// LogFailoverEvent logs a failover-related event with standard fields
func (l *Logger) LogFailoverEvent(event string, oldMaster, newMaster string, fields ...map[string]interface{}) {
	baseFields := map[string]interface{}{
		"event":      event,
		"old_master": oldMaster,
		"new_master": newMaster,
	}
	
	allFields := []map[string]interface{}{baseFields}
	allFields = append(allFields, fields...)
	
	l.Warn(fmt.Sprintf("Failover %s: %s -> %s", event, oldMaster, newMaster), allFields...)
}