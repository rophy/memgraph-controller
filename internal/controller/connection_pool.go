package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"memgraph-controller/internal/common"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type ConnectionPool struct {
	drivers map[string]neo4j.DriverWithContext // boltAddress -> driver
	podIPs  map[string]string                  // podName -> currentIP
	mutex   sync.RWMutex
	config  *common.Config
}

func NewConnectionPool(config *common.Config) *ConnectionPool {
	return &ConnectionPool{
		drivers: make(map[string]neo4j.DriverWithContext),
		podIPs:  make(map[string]string),
		config:  config,
	}
}

func (cp *ConnectionPool) GetDriver(ctx context.Context, boltAddress string) (neo4j.DriverWithContext, error) {
	logger := common.GetLoggerFromContext(ctx)
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
			logger.Warn("driver is no longer valid", "bolt_address", boltAddress, "error", err)
			cp.removeDriver(boltAddress)
		}
	}

	// Create new driver
	return cp.createDriver(ctx, boltAddress)
}

func (cp *ConnectionPool) createDriver(ctx context.Context, boltAddress string) (neo4j.DriverWithContext, error) {
	logger := common.GetLoggerFromContext(ctx)
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
	logger.Debug("created new driver", "bolt_address", boltAddress)
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
func (cp *ConnectionPool) UpdatePodIP(ctx context.Context, podName, newIP string) {
	logger := common.GetLoggerFromContext(ctx)
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
func (cp *ConnectionPool) InvalidatePodConnection(ctx context.Context, podName string) {
	logger := common.GetLoggerFromContext(ctx)
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	if ip, exists := cp.podIPs[podName]; exists {
		boltAddress := ip + ":7687"
		if driver, exists := cp.drivers[boltAddress]; exists {
			driver.Close(context.Background())
			delete(cp.drivers, boltAddress)
			logger.Debug("invalidated connection: IP changed", "pod_name", podName, "old_ip", ip)
		}
	}
}

// InvalidateConnection invalidates a connection by bolt address
func (cp *ConnectionPool) InvalidateConnection(boltAddress string) error {
	return cp.removeDriver(boltAddress)
}

func (cp *ConnectionPool) Close(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
	cp.mutex.Lock()
	defer cp.mutex.Unlock()

	for boltAddress, driver := range cp.drivers {
		driver.Close(ctx)
		logger.Debug("closed driver", "bolt_address", boltAddress)
	}
	cp.drivers = make(map[string]neo4j.DriverWithContext)
	cp.podIPs = make(map[string]string)
}

// RunQueryWithTimeout runs a query with a guaranteed timeout
func (cp *ConnectionPool) RunQueryWithTimeout(ctx context.Context, boltAddress string, query string, timeout time.Duration) ([]*neo4j.Record, error) {
	logger := common.GetLoggerFromContext(ctx)

	if boltAddress == "" {
		return nil, fmt.Errorf("bolt address is empty")
	}

	type queryResult struct {
		records []*neo4j.Record
		err     error
	}

	resultChan := make(chan queryResult, 1)

	// Run query in goroutine
	go func() {
		records, err := cp.RunQuery(ctx, boltAddress, query)
		resultChan <- queryResult{
			records: records,
			err:     err,
		}
	}()

	// Guaranteed timeout using select
	select {
	case res := <-resultChan:
		return res.records, res.err
	case <-time.After(timeout):
		logger.Warn("RunQueryWithTimeout timed out", "bolt_address", boltAddress, "query", query, "timeout", timeout)
		return nil, fmt.Errorf("bolt query timed out after %v", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RunQuery runs a query and returns collected records
func (cp *ConnectionPool) RunQuery(ctx context.Context, boltAddress string, query string) ([]*neo4j.Record, error) {
	logger := common.GetLoggerFromContext(ctx)

	driver, err := cp.GetDriver(ctx, boltAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
	}

	// Create session
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer func() {
		if closeErr := session.Close(ctx); closeErr != nil {
			logger.Warn("Failed to close session", "bolt_address", boltAddress, "error", closeErr)
		}
	}()

	// Run query and collect all records
	result, err := session.Run(ctx, query, nil)
	if err != nil {
		return nil, err
	}

	// Collect all records while session is still open
	records, err := result.Collect(ctx)
	return records, err
}
