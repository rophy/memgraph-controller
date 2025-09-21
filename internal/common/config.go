package common

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration
type Config struct {
	// Application
	AppName         string
	Namespace       string
	StatefulSetName string

	// Controller
	ReconcileInterval time.Duration
	HTTPPort          string
	BoltPort          string
	ReplicationPort   string

	// Gateway
	GatewayBindAddress string

	// Logging
	LogLevel     string
	LogAddSource bool
	LogFormat    string
}

// Load loads configuration from environment variables with defaults
func Load() (*Config, error) {
	reconcileInterval, err := time.ParseDuration(getEnvOrDefault("RECONCILE_INTERVAL", "30s"))
	if err != nil {
		reconcileInterval = 30 * time.Second
	}

	config := &Config{
		// Application
		AppName:         os.Getenv("APP_NAME"),
		Namespace:       os.Getenv("NAMESPACE"),
		StatefulSetName: os.Getenv("STATEFULSET_NAME"),

		// Controller
		ReconcileInterval: reconcileInterval,
		HTTPPort:          getEnvOrDefault("HTTP_PORT", "8080"),
		BoltPort:          getEnvOrDefault("BOLT_PORT", "7687"),
		ReplicationPort:   getEnvOrDefault("REPLICATION_PORT", "10000"),

		// Gateway
		GatewayBindAddress: getEnvOrDefault("GATEWAY_BIND_ADDRESS", "0.0.0.0:7687"),

		// Logging
		LogLevel:     getEnvOrDefault("LOG_LEVEL", "info"),
		LogAddSource: getBoolEnvOrDefault("LOG_ADD_SOURCE", true),
		LogFormat:    getEnvOrDefault("LOG_FORMAT", "text"),
	}

	return config, nil
}

// Controller config methods
func (c *Config) IsMemgraphPod(podName string) bool {
	return strings.HasPrefix(podName, c.StatefulSetName+"-")
}

func (c *Config) GetPodName(index int32) string {
	return fmt.Sprintf("%s-%d", c.StatefulSetName, index)
}

func (c *Config) GetEligiblePodNames() (string, string) {
	return c.GetPodName(0), c.GetPodName(1)
}

func (c *Config) ExtractPodIndex(podName string) int {
	lastDash := strings.LastIndex(podName, "-")
	if lastDash == -1 {
		return -1
	}

	indexStr := podName[lastDash+1:]
	if index, err := strconv.Atoi(indexStr); err == nil {
		return index
	}

	return -1
}

// Helper functions
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getBoolEnvOrDefault(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		switch strings.ToLower(value) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		}
	}
	return defaultValue
}
