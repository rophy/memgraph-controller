package controller

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	AppName            string
	Namespace          string
	ReconcileInterval  time.Duration
	BoltPort           string
	ReplicationPort    string
	ServiceName        string
	HTTPPort           string
}

func LoadConfig() *Config {
	reconcileInterval, err := time.ParseDuration(getEnvOrDefault("RECONCILE_INTERVAL", "30s"))
	if err != nil {
		reconcileInterval = 30 * time.Second
	}

	return &Config{
		AppName:            getEnvOrDefault("APP_NAME", "memgraph"),
		Namespace:          getEnvOrDefault("NAMESPACE", "memgraph"),
		ReconcileInterval:  reconcileInterval,
		BoltPort:           getEnvOrDefault("BOLT_PORT", "7687"),
		ReplicationPort:    getEnvOrDefault("REPLICATION_PORT", "10000"),
		ServiceName:        getEnvOrDefault("SERVICE_NAME", "memgraph"),
		HTTPPort:           getEnvOrDefault("HTTP_PORT", "8080"),
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvOrDefaultInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}