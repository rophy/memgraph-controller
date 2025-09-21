package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"memgraph-controller/internal/common"
)

// ProberConfig holds configuration for the health prober
type ProberConfig struct {
	// CheckInterval is the interval between health checks
	CheckInterval time.Duration
	// Timeout is the timeout for each individual health check
	Timeout time.Duration
	// FailureThreshold is the number of consecutive failures before triggering failover
	FailureThreshold int
}

// DefaultProberConfig returns default configuration for the health prober
func DefaultProberConfig() ProberConfig {
	return ProberConfig{
		CheckInterval:    5 * time.Second,
		Timeout:          3 * time.Second,
		FailureThreshold: 3,
	}
}

// HealthProber monitors the health of the main Memgraph pod and read gateway upstream
type HealthProber struct {
	controller          *MemgraphController
	config              ProberConfig
	mu                  sync.RWMutex
	runningCh           chan struct{}
	consecutiveFailures int
	lastHealthStatus    bool

	// Read gateway upstream health tracking (independent from main pod health)
	readGatewayConsecutiveFailures int
	lastReadGatewayHealthStatus    bool
}

// NewHealthProber creates a new health prober instance
func NewHealthProber(controller *MemgraphController, config ProberConfig) *HealthProber {
	return &HealthProber{
		controller:                     controller,
		config:                         config,
		runningCh:                      nil, // Will be initialized in Start()
		consecutiveFailures:            0,
		lastHealthStatus:               true,
		readGatewayConsecutiveFailures: 0,
		lastReadGatewayHealthStatus:    true,
	}
}

// Start begins the health checking goroutine
func (p *HealthProber) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.runningCh != nil {
		common.GetLogger().Warn("Health prober already running")
		return
	}

	common.GetLogger().Info("Starting health prober",
		"check_interval", p.config.CheckInterval,
		"timeout", p.config.Timeout,
		"failure_threshold", p.config.FailureThreshold)
	p.runningCh = make(chan struct{})
	go p.runHealthCheckLoop(ctx)
}

// Stop stops the health checking goroutine
func (p *HealthProber) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.runningCh == nil {
		return
	}

	common.GetLogger().Info("Stopping health prober")
	close(p.runningCh)
	p.runningCh = nil
}

// IsRunning returns whether the prober is currently running
func (p *HealthProber) IsRunning() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.runningCh != nil
}

// GetHealthStatus returns the current health status and consecutive failure count
func (p *HealthProber) GetHealthStatus() (healthy bool, consecutiveFailures int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastHealthStatus, p.consecutiveFailures
}

// runHealthCheckLoop is the main health checking loop
func (p *HealthProber) runHealthCheckLoop(ctx context.Context) {
	ctx, logger := common.WithAttr(ctx, "thread", "healthCheck")

	ticker := time.NewTicker(p.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Health prober stopped due to context cancellation")
			return
		case <-p.runningCh:
			logger.Info("Health prober stopped")
			return
		case <-ticker.C:
			p.performHealthCheck(ctx)
		}
	}
}

// performHealthCheck performs a single health check against the main pod and read gateway upstream
func (p *HealthProber) performHealthCheck(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
	// Create timeout context for this specific health check
	checkCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()

	// Get current target main pod
	targetMainIndex, err := p.controller.GetTargetMainIndex(checkCtx)
	if err != nil {
		logger.Error("Health check failed: cannot get target main index", "error", err)
		p.recordFailure(ctx)
		return
	}

	targetMainPodName := p.controller.config.GetPodName(targetMainIndex)

	// Perform blackbox health check - try to ping the main pod
	targetMainNode, err := p.controller.getTargetMainNode(checkCtx)
	if err != nil || targetMainNode == nil {
		logger.Error("Health check failed: cannot get main node", "pod_name", targetMainPodName, "error", err)
		p.recordFailure(ctx)
		return
	}

	err = targetMainNode.Ping(checkCtx)
	if err != nil {
		logger.Warn("Health check failed: cannot ping main pod",
			"pod_name", targetMainPodName,
			"error", err,
			"consecutive_failures", p.consecutiveFailures+1,
			"failure_threshold", p.config.FailureThreshold)
		p.recordFailure(ctx)
		return
	}

	// Health check succeeded for main pod
	p.recordSuccess(ctx, targetMainPodName)

	// Also perform read gateway upstream health check (independent from main pod health)
	p.performReadGatewayUpstreamHealthCheck(ctx)
}

// recordFailure records a health check failure and triggers failover if threshold is reached
func (p *HealthProber) recordFailure(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()

	p.consecutiveFailures++
	p.lastHealthStatus = false

	// Check if we've reached the failure threshold
	if p.consecutiveFailures == p.config.FailureThreshold {
		logger.Error("🚨 Health prober: failure threshold reached, triggering failover",
			"consecutive_failures", p.consecutiveFailures,
			"failure_threshold", p.config.FailureThreshold)

		// Trigger failover by submitting event to failoverCheckQueue
		// We use a separate goroutine to avoid blocking the health check loop
		go p.triggerFailover(ctx)

		// Don't reset the counter - we should only trigger failover once
		// The counter will be reset when the pod recovers (recordSuccess)
	}
}

// recordSuccess records a health check success
func (p *HealthProber) recordSuccess(ctx context.Context, podName string) {
	logger := common.GetLoggerFromContext(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()

	wasUnhealthy := p.consecutiveFailures > 0
	p.consecutiveFailures = 0
	p.lastHealthStatus = true

	if wasUnhealthy {
		logger.Info("✅ Health prober: main pod recovered",
			"pod_name", podName,
			"previous_failures", p.consecutiveFailures)
	} else {
		logger.Debug("Health check successful", "pod_name", podName)
	}
}

// triggerFailover triggers a failover by submitting event to failoverCheckQueue
func (p *HealthProber) triggerFailover(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
	// Guard against nil controller (for testing)
	if p.controller == nil {
		logger.Warn("Health prober: cannot trigger failover with nil controller")
		return
	}

	// Guard against nil failoverCheckQueue
	if p.controller.failoverCheckQueue == nil || p.controller.failoverCheckQueue.events == nil {
		logger.Warn("Health prober: failoverCheckQueue not initialized")
		return
	}

	logger.Info("Health prober: queueing failover check event")

	// Get current target main pod name
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	targetMainIndex, err := p.controller.GetTargetMainIndex(ctx)
	if err != nil {
		logger.Error("Health prober: cannot get target main index", "error", err)
		return
	}
	targetMainPodName := p.controller.config.GetPodName(targetMainIndex)

	// Submit event to failoverCheckQueue instead of direct execution
	// This ensures proper synchronization and deduplication
	event := FailoverCheckEvent{
		Reason:    "health-check-failure",
		PodName:   targetMainPodName,
		Timestamp: time.Now(),
	}

	// Non-blocking send to avoid deadlock if queue is full
	select {
	case p.controller.failoverCheckQueue.events <- event:
		common.GetLogger().Info("Health prober: successfully queued failover check event")
	default:
		common.GetLogger().Error("Health prober: failover check queue is full, event dropped")
	}
}

// performReadGatewayUpstreamHealthCheck performs health check on read gateway upstream
func (p *HealthProber) performReadGatewayUpstreamHealthCheck(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)

	// Skip if read gateway is not enabled
	if p.controller.readGatewayServer == nil {
		return
	}

	// Get current read gateway upstream
	currentUpstream := p.controller.readGatewayServer.GetUpstreamAddress()
	if currentUpstream == "" {
		// No upstream set - reset failure count
		p.resetReadGatewayFailures(ctx)
		return
	}

	// Create timeout context for this specific health check
	checkCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()

	// Find the upstream node and ping it
	upstreamNode, err := p.findUpstreamNode(currentUpstream)
	if err != nil {
		logger.Debug("read gateway health check: could not find upstream node",
			"upstream_address", currentUpstream,
			"error", err)
		p.recordReadGatewayFailure(ctx, currentUpstream)
		return
	}

	// Ping the upstream node
	err = upstreamNode.Ping(checkCtx)
	if err != nil {
		logger.Debug("read gateway health check: upstream ping failed",
			"upstream_address", currentUpstream,
			"error", err,
			"consecutive_failures", p.readGatewayConsecutiveFailures+1,
			"failure_threshold", p.config.FailureThreshold)
		p.recordReadGatewayFailure(ctx, currentUpstream)
		return
	}

	// Health check succeeded
	p.recordReadGatewaySuccess(ctx, currentUpstream)
}

// findUpstreamNode finds the MemgraphNode corresponding to the upstream address
func (p *HealthProber) findUpstreamNode(upstreamAddress string) (*MemgraphNode, error) {
	// Validate upstream address format
	if len(upstreamAddress) <= 5 || upstreamAddress[len(upstreamAddress)-5:] != ":7687" {
		return nil, fmt.Errorf("invalid upstream address format: %s", upstreamAddress)
	}

	// Find the node with matching address
	for _, node := range p.controller.cluster.MemgraphNodes {
		if nodeAddress, err := node.GetBoltAddress(); err == nil {
			if nodeAddress == upstreamAddress {
				return node, nil
			}
		}
	}

	return nil, fmt.Errorf("upstream node not found for address: %s", upstreamAddress)
}

// recordReadGatewayFailure records a read gateway upstream health check failure
func (p *HealthProber) recordReadGatewayFailure(ctx context.Context, upstreamAddress string) {
	logger := common.GetLoggerFromContext(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()

	p.readGatewayConsecutiveFailures++
	p.lastReadGatewayHealthStatus = false

	// Check if we've reached the failure threshold
	if p.readGatewayConsecutiveFailures == p.config.FailureThreshold {
		logger.Warn("🔄 READ GATEWAY UPSTREAM FAILURE: threshold reached, switching upstream",
			"upstream_address", upstreamAddress,
			"consecutive_failures", p.readGatewayConsecutiveFailures,
			"failure_threshold", p.config.FailureThreshold)

		// Trigger immediate upstream switching
		go p.triggerReadGatewayUpstreamSwitch(ctx, upstreamAddress)

		// Don't reset the counter - will be reset when switch succeeds
	}
}

// recordReadGatewaySuccess records a read gateway upstream health check success
func (p *HealthProber) recordReadGatewaySuccess(ctx context.Context, upstreamAddress string) {
	logger := common.GetLoggerFromContext(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()

	wasUnhealthy := p.readGatewayConsecutiveFailures > 0
	p.readGatewayConsecutiveFailures = 0
	p.lastReadGatewayHealthStatus = true

	if wasUnhealthy {
		logger.Info("✅ Read gateway upstream recovered",
			"upstream_address", upstreamAddress,
			"previous_failures", p.readGatewayConsecutiveFailures)
	} else {
		logger.Debug("Read gateway upstream health check successful", "upstream_address", upstreamAddress)
	}
}

// resetReadGatewayFailures resets read gateway failure counters
func (p *HealthProber) resetReadGatewayFailures(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.readGatewayConsecutiveFailures > 0 {
		p.readGatewayConsecutiveFailures = 0
		p.lastReadGatewayHealthStatus = true
	}
}

// triggerReadGatewayUpstreamSwitch triggers switching of read gateway upstream
func (p *HealthProber) triggerReadGatewayUpstreamSwitch(ctx context.Context, failedUpstream string) {
	logger := common.GetLoggerFromContext(ctx)

	// Guard against nil controller (for testing)
	if p.controller == nil {
		logger.Warn("Health prober: cannot switch read gateway upstream with nil controller")
		return
	}

	logger.Info("Health prober: switching read gateway upstream due to health failures",
		"failed_upstream", failedUpstream)

	// Create timeout context for upstream switching
	switchCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Trigger upstream switching
	p.controller.updateReadGatewayUpstream(switchCtx)

	newUpstream := p.controller.readGatewayServer.GetUpstreamAddress()
	if newUpstream != failedUpstream {
		logger.Info("Read gateway upstream switch completed",
			"old_upstream", failedUpstream,
			"new_upstream", newUpstream)
		// Reset failure counter after successful switch
		p.resetReadGatewayFailures(ctx)
	} else {
		logger.Warn("Read gateway upstream switch failed - no alternative upstream available",
			"failed_upstream", failedUpstream)
	}
}
