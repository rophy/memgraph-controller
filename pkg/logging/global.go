package logging

import "sync"

var (
	globalLogger *Logger
	once         sync.Once
)

// InitLogger initializes the global logger (call once at startup)
func InitLogger() {
	once.Do(func() {
		globalLogger = NewLogger(Config{
			Level: GetLogLevel(),
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