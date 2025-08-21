package controller

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	StatefulSetName    string
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
		StatefulSetName:    getEnvOrDefault("STATEFULSET_NAME", "memgraph"),
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

// GetPodName returns the name of a pod given its index (e.g., "my-release-memgraph-0")
func (c *Config) GetPodName(index int) string {
	return fmt.Sprintf("%s-%d", c.StatefulSetName, index)
}

// GetEligiblePodNames returns the names of pods eligible for master/SYNC roles (pod-0 and pod-1)
func (c *Config) GetEligiblePodNames() (string, string) {
	return c.GetPodName(0), c.GetPodName(1)
}

// ExtractPodIndex extracts the index from a pod name (e.g., "my-release-memgraph-2" -> 2)
func (c *Config) ExtractPodIndex(podName string) int {
	// Find the last dash and extract the number after it
	lastDash := strings.LastIndex(podName, "-")
	if lastDash == -1 {
		return -1 // Invalid pod name format
	}
	
	indexStr := podName[lastDash+1:]
	if index, err := strconv.Atoi(indexStr); err == nil {
		return index
	}
	
	return -1 // Could not parse index
}