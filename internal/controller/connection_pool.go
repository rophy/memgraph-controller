package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"memgraph-controller/internal/common"
)

type ConnectionPool struct {
	drivers map[string]neo4j.DriverWithContext // boltAddress -> driver
	podIPs  map[string]string                  // podName -> currentIP
	mutex   sync.RWMutex
	config  *common.Config
}

type RetryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

func NewConnectionPool(config *common.Config) *ConnectionPool {
	return &ConnectionPool{
		drivers: make(map[string]neo4j.DriverWithContext),
		podIPs:  make(map[string]string),
		config:  config,
	}
}

func (cp *ConnectionPool) GetDriver(ctx context.Context, boltAddress string) (neo4j.DriverWithContext, error) {
	if boltAddress == "" {
		return nil, fmt.Errorf("bolt address is empty")
	}

	cp.mutex.RLock()
	driver, exists := cp.drivers[boltAddress]
	cp.mutex.RUnlock()

	if exists {
		// Test if existing driver is still valid
		if err := driver.VerifyConnectivity(ctx); err == nil {
			return driver, nil
		} else {
			// Driver is no longer valid, remove it and create a new one
			logger.Warn("driver for %s is no longer valid", "bolt_address", boltAddress, "error", err)
			cp.removeDriver(boltAddress)
		}
	}

	// Create new driver
	return cp.createDriver(ctx, boltAddress)
}

func (cp *ConnectionPool) createDriver(ctx context.Context, boltAddress string) (neo4j.DriverWithContext, error) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	// Double-check that another goroutine didn't create the driver
	if driver, exists := cp.drivers[boltAddress]; exists {
		return driver, nil
	}

	driver, err := neo4j.NewDriverWithContext(
		fmt.Sprintf("bolt://%s", boltAddress),
		neo4j.BasicAuth("memgraph", "", ""),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create driver for %s: %w", boltAddress, err)
	}

	// Test connectivity
	if err := driver.VerifyConnectivity(ctx); err != nil {
		driver.Close(ctx)
		return nil, fmt.Errorf("failed to verify connectivity to %s: %w", boltAddress, err)
	}

	cp.drivers[boltAddress] = driver
	logger.Debug("created new driver for %s", "bolt_address", boltAddress)
	return driver, nil
}

func (cp *ConnectionPool) removeDriver(boltAddress string) error {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	if driver, exists := cp.drivers[boltAddress]; exists {
		err := driver.Close(context.Background())
		if err != nil {
			return fmt.Errorf("failed to close driver for %s: %w", boltAddress, err)
		}
		delete(cp.drivers, boltAddress)
	}
	return nil
}

// UpdatePodIP tracks pod IP changes and invalidates connections when IP changes
func (cp *ConnectionPool) UpdatePodIP(podName, newIP string) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	if existingIP, exists := cp.podIPs[podName]; exists && existingIP != newIP {
		// IP changed - invalidate old connection
		oldBoltAddress := existingIP + ":7687"
		if driver, exists := cp.drivers[oldBoltAddress]; exists {
			driver.Close(context.Background())
			delete(cp.drivers, oldBoltAddress)
			logger.Debug("invalidated connection for pod", "pod", podName, "old_ip", existingIP, "new_ip", newIP)
		}
	}

	cp.podIPs[podName] = newIP
}

// InvalidatePodConnection removes connections for a specific pod
func (cp *ConnectionPool) InvalidatePodConnection(podName string) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	if ip, exists := cp.podIPs[podName]; exists {
		boltAddress := ip + ":7687"
		if driver, exists := cp.drivers[boltAddress]; exists {
			driver.Close(context.Background())
			delete(cp.drivers, boltAddress)
			logger.Debug("invalidated connection for pod %s: IP changed from %s to %s", podName, ip)
		}
	}
}

// InvalidateConnection invalidates a connection by bolt address
func (cp *ConnectionPool) InvalidateConnection(boltAddress string) error {
	return cp.removeDriver(boltAddress)
}

func (cp *ConnectionPool) Close(ctx context.Context) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	for boltAddress, driver := range cp.drivers {
		driver.Close(ctx)
		logger.Debug("closed driver for %s", "bolt_address", boltAddress)
	}
	cp.drivers = make(map[string]neo4j.DriverWithContext)
	cp.podIPs = make(map[string]string)
}

func WithRetry(ctx context.Context, operation func() error, retryConfig RetryConfig) error {
	var lastErr error

	for attempt := 0; attempt <= retryConfig.MaxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := operation()
		if err == nil {
			return nil
		}

		lastErr = err

		if attempt == retryConfig.MaxRetries {
			break
		}

		// Calculate delay with exponential backoff
		delay := retryConfig.BaseDelay * time.Duration(1<<uint(attempt))
		if delay > retryConfig.MaxDelay {
			delay = retryConfig.MaxDelay
		}

		logger.Warn("operation failed", "attempt", attempt+1, "max_retries", retryConfig.MaxRetries+1, "error", err, "delay", delay)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	return fmt.Errorf("operation failed after %d attempts: %w", retryConfig.MaxRetries+1, lastErr)
}

// WithRetryAndRefresh performs retry with pod IP refresh on connection failures
func WithRetryAndRefresh(ctx context.Context, operation func() error, retryConfig RetryConfig, refreshFunc func() error) error {
	var lastErr error

	for attempt := 0; attempt <= retryConfig.MaxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		err := operation()
		if err == nil {
			return nil
		}

		lastErr = err

		if attempt == retryConfig.MaxRetries {
			break
		}

		// On connectivity errors, refresh pod information before retry
		if strings.Contains(err.Error(), "ConnectivityError") || strings.Contains(err.Error(), "i/o timeout") {
			if refreshFunc != nil {
				if refreshErr := refreshFunc(); refreshErr != nil {
					logger.Warn("failed to refresh pod info", "error", refreshErr)
				}
			}
		}

		// Calculate delay with exponential backoff
		delay := retryConfig.BaseDelay * time.Duration(1<<uint(attempt))
		if delay > retryConfig.MaxDelay {
			delay = retryConfig.MaxDelay
		}

		logger.Warn("operation failed", "attempt", attempt+1, "max_retries", retryConfig.MaxRetries+1, "error", err, "delay", delay)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}

	return fmt.Errorf("operation failed after %d attempts: %w", retryConfig.MaxRetries+1, lastErr)
}
