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
	BindAddress    string
	MaxConnections int
	Timeout        time.Duration
	BufferSize     int

	// Enhanced connection management settings
	HealthCheckInterval   time.Duration
	ConnectionTimeout     time.Duration
	IdleTimeout           time.Duration
	MaxBytesPerConnection int64
	CleanupInterval       time.Duration

	// Production readiness settings
	TLSEnabled       bool
	TLSCertPath      string
	TLSKeyPath       string
	RateLimitEnabled bool
	RateLimitRPS     int           // Requests per second per IP
	RateLimitBurst   int           // Burst size
	RateLimitWindow  time.Duration // Time window for rate limiting
	LogLevel         string        // debug, info, warn, error
	TraceEnabled     bool          // Enable distributed tracing
}

// LoadGatewayConfig loads gateway configuration from environment variables
func LoadGatewayConfig() *Config {
	maxConnections := getEnvOrDefaultInt("GATEWAY_MAX_CONNECTIONS", 1000)

	timeout, err := time.ParseDuration(getEnvOrDefault("GATEWAY_TIMEOUT", "30s"))
	if err != nil {
		timeout = 30 * time.Second
	}

	healthCheckInterval, err := time.ParseDuration(getEnvOrDefault("GATEWAY_HEALTH_CHECK_INTERVAL", "30s"))
	if err != nil {
		healthCheckInterval = 30 * time.Second
	}

	connectionTimeout, err := time.ParseDuration(getEnvOrDefault("GATEWAY_CONNECTION_TIMEOUT", "10s"))
	if err != nil {
		connectionTimeout = 10 * time.Second
	}

	idleTimeout, err := time.ParseDuration(getEnvOrDefault("GATEWAY_IDLE_TIMEOUT", "5m"))
	if err != nil {
		idleTimeout = 5 * time.Minute
	}

	cleanupInterval, err := time.ParseDuration(getEnvOrDefault("GATEWAY_CLEANUP_INTERVAL", "1m"))
	if err != nil {
		cleanupInterval = 1 * time.Minute
	}

	bufferSize := getEnvOrDefaultInt("GATEWAY_BUFFER_SIZE", 32768)                                // 32KB default
	maxBytesPerConnection := getEnvOrDefaultInt64("GATEWAY_MAX_BYTES_PER_CONNECTION", 1048576000) // 1GB default

	// Production settings
	tlsEnabled := getEnvOrDefaultBool("GATEWAY_TLS_ENABLED", false)
	rateLimitEnabled := getEnvOrDefaultBool("GATEWAY_RATE_LIMIT_ENABLED", false)
	rateLimitRPS := getEnvOrDefaultInt("GATEWAY_RATE_LIMIT_RPS", 100)
	rateLimitBurst := getEnvOrDefaultInt("GATEWAY_RATE_LIMIT_BURST", 200)

	rateLimitWindow, err := time.ParseDuration(getEnvOrDefault("GATEWAY_RATE_LIMIT_WINDOW", "1m"))
	if err != nil {
		rateLimitWindow = 1 * time.Minute
	}

	traceEnabled := getEnvOrDefaultBool("GATEWAY_TRACE_ENABLED", false)

	return &Config{
		BindAddress:           getEnvOrDefault("GATEWAY_BIND_ADDRESS", "0.0.0.0:7687"),
		MaxConnections:        maxConnections,
		Timeout:               timeout,
		BufferSize:            bufferSize,
		HealthCheckInterval:   healthCheckInterval,
		ConnectionTimeout:     connectionTimeout,
		IdleTimeout:           idleTimeout,
		MaxBytesPerConnection: maxBytesPerConnection,
		CleanupInterval:       cleanupInterval,
		TLSEnabled:            tlsEnabled,
		TLSCertPath:           getEnvOrDefault("GATEWAY_TLS_CERT_PATH", "/etc/certs/tls.crt"),
		TLSKeyPath:            getEnvOrDefault("GATEWAY_TLS_KEY_PATH", "/etc/certs/tls.key"),
		RateLimitEnabled:      rateLimitEnabled,
		RateLimitRPS:          rateLimitRPS,
		RateLimitBurst:        rateLimitBurst,
		RateLimitWindow:       rateLimitWindow,
		LogLevel:              getEnvOrDefault("GATEWAY_LOG_LEVEL", "info"),
		TraceEnabled:          traceEnabled,
	}
}

// Validate checks if the gateway configuration is valid
func (c *Config) Validate() error {

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

	// Validate health check interval
	if c.HealthCheckInterval < 5*time.Second {
		return fmt.Errorf("health check interval too short %v, minimum is 5 seconds", c.HealthCheckInterval)
	}
	if c.HealthCheckInterval > 10*time.Minute {
		return fmt.Errorf("health check interval too long %v, maximum is 10 minutes", c.HealthCheckInterval)
	}

	// Validate connection timeout
	if c.ConnectionTimeout < time.Second {
		return fmt.Errorf("connection timeout too short %v, minimum is 1 second", c.ConnectionTimeout)
	}
	if c.ConnectionTimeout > c.Timeout {
		return fmt.Errorf("connection timeout %v cannot be longer than backend timeout %v", c.ConnectionTimeout, c.Timeout)
	}

	// Validate idle timeout
	if c.IdleTimeout < time.Minute {
		return fmt.Errorf("idle timeout too short %v, minimum is 1 minute", c.IdleTimeout)
	}
	if c.IdleTimeout > 24*time.Hour {
		return fmt.Errorf("idle timeout too long %v, maximum is 24 hours", c.IdleTimeout)
	}

	// Validate cleanup interval
	if c.CleanupInterval < 10*time.Second {
		return fmt.Errorf("cleanup interval too short %v, minimum is 10 seconds", c.CleanupInterval)
	}
	if c.CleanupInterval > time.Hour {
		return fmt.Errorf("cleanup interval too long %v, maximum is 1 hour", c.CleanupInterval)
	}

	// Validate max bytes per connection
	if c.MaxBytesPerConnection < 1048576 { // 1MB minimum
		return fmt.Errorf("max bytes per connection too small %d, minimum is 1048576 bytes", c.MaxBytesPerConnection)
	}
	if c.MaxBytesPerConnection > 107374182400 { // 100GB maximum
		return fmt.Errorf("max bytes per connection too large %d, maximum is 107374182400 bytes", c.MaxBytesPerConnection)
	}

	// Validate TLS configuration
	if c.TLSEnabled {
		if c.TLSCertPath == "" || c.TLSKeyPath == "" {
			return fmt.Errorf("TLS enabled but cert path (%s) or key path (%s) is empty", c.TLSCertPath, c.TLSKeyPath)
		}
	}

	// Validate rate limiting configuration
	if c.RateLimitEnabled {
		if c.RateLimitRPS <= 0 {
			return fmt.Errorf("rate limit RPS must be positive, got %d", c.RateLimitRPS)
		}
		if c.RateLimitBurst <= 0 {
			return fmt.Errorf("rate limit burst must be positive, got %d", c.RateLimitBurst)
		}
		if c.RateLimitWindow <= 0 {
			return fmt.Errorf("rate limit window must be positive, got %v", c.RateLimitWindow)
		}
	}

	// Validate log level
	validLogLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("invalid log level %s, must be one of: debug, info, warn, error", c.LogLevel)
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

func getEnvOrDefaultInt64(key string, defaultValue int64) int64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return defaultValue
}
