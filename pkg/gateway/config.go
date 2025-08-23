package gateway

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// Config holds the gateway configuration
type Config struct {
	Enabled        bool
	BindAddress    string
	MaxConnections int
	Timeout        time.Duration
	BufferSize     int
}

// LoadGatewayConfig loads gateway configuration from environment variables
func LoadGatewayConfig() *Config {
	enabled := getEnvOrDefaultBool("GATEWAY_ENABLED", false)
	maxConnections := getEnvOrDefaultInt("GATEWAY_MAX_CONNECTIONS", 1000)
	
	timeout, err := time.ParseDuration(getEnvOrDefault("GATEWAY_TIMEOUT", "30s"))
	if err != nil {
		timeout = 30 * time.Second
	}
	
	bufferSize := getEnvOrDefaultInt("GATEWAY_BUFFER_SIZE", 32768) // 32KB default
	
	return &Config{
		Enabled:        enabled,
		BindAddress:    getEnvOrDefault("GATEWAY_BIND_ADDRESS", "0.0.0.0:7687"),
		MaxConnections: maxConnections,
		Timeout:        timeout,
		BufferSize:     bufferSize,
	}
}

// Validate checks if the gateway configuration is valid
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil // Skip validation if disabled
	}
	
	// Validate bind address
	host, port, err := net.SplitHostPort(c.BindAddress)
	if err != nil {
		return fmt.Errorf("invalid bind address %s: %v", c.BindAddress, err)
	}
	
	// Validate port range
	portNum, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("invalid port in bind address %s: %v", c.BindAddress, err)
	}
	if portNum < 1 || portNum > 65535 {
		return fmt.Errorf("port %d out of valid range (1-65535)", portNum)
	}
	
	// Validate IP address (if not wildcard)
	if host != "0.0.0.0" && host != "" {
		if ip := net.ParseIP(host); ip == nil {
			return fmt.Errorf("invalid IP address %s in bind address", host)
		}
	}
	
	// Validate max connections
	if c.MaxConnections < 1 {
		return fmt.Errorf("max connections must be at least 1, got %d", c.MaxConnections)
	}
	if c.MaxConnections > 100000 {
		return fmt.Errorf("max connections too high %d, maximum allowed is 100000", c.MaxConnections)
	}
	
	// Validate timeout
	if c.Timeout < time.Second {
		return fmt.Errorf("timeout too short %v, minimum is 1 second", c.Timeout)
	}
	if c.Timeout > 5*time.Minute {
		return fmt.Errorf("timeout too long %v, maximum is 5 minutes", c.Timeout)
	}
	
	// Validate buffer size
	if c.BufferSize < 1024 {
		return fmt.Errorf("buffer size too small %d, minimum is 1024 bytes", c.BufferSize)
	}
	if c.BufferSize > 1048576 { // 1MB max
		return fmt.Errorf("buffer size too large %d, maximum is 1048576 bytes", c.BufferSize)
	}
	
	return nil
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

func getEnvOrDefaultBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	}
	return defaultValue
}