package controller

import (
	"context"
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

// HealthProber monitors the health of the main Memgraph pod and triggers failover when needed
type HealthProber struct {
	controller       *MemgraphController
	config           ProberConfig
	mu               sync.RWMutex
	running          bool
	stopCh           chan struct{}
	consecutiveFailures int
	lastHealthStatus bool
}

// NewHealthProber creates a new health prober instance
func NewHealthProber(controller *MemgraphController, config ProberConfig) *HealthProber {
	return &HealthProber{
		controller:          controller,
		config:              config,
		running:             false,
		stopCh:              make(chan struct{}),
		consecutiveFailures: 0,
		lastHealthStatus:    true,
	}
}

// Start begins the health checking goroutine
func (p *HealthProber) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	
	if p.running {
		common.GetLogger().Warn("Health prober already running")
		return
	}
	
	p.running = true
	common.GetLogger().Info("Starting health prober", 
		"check_interval", p.config.CheckInterval,
		"timeout", p.config.Timeout,
		"failure_threshold", p.config.FailureThreshold)
	
	go p.runHealthCheckLoop(ctx)
}

// Stop stops the health checking goroutine
func (p *HealthProber) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	
	if !p.running {
		return
	}
	
	common.GetLogger().Info("Stopping health prober")
	p.running = false
	close(p.stopCh)
}

// IsRunning returns whether the prober is currently running
func (p *HealthProber) IsRunning() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.running
}

// GetHealthStatus returns the current health status and consecutive failure count
func (p *HealthProber) GetHealthStatus() (healthy bool, consecutiveFailures int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastHealthStatus, p.consecutiveFailures
}

// runHealthCheckLoop is the main health checking loop
func (p *HealthProber) runHealthCheckLoop(ctx context.Context) {
	// Add goroutine context for health checker
	ctx = context.WithValue(ctx, goroutineKey, "health-check")
	logger := common.GetLogger().WithContext(ctx)
	ctx = common.WithLogger(ctx, logger)
	
	ticker := time.NewTicker(p.config.CheckInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			common.GetLogger().Info("Health prober stopped due to context cancellation")
			return
		case <-p.stopCh:
			common.GetLogger().Info("Health prober stopped")
			return
		case <-ticker.C:
			p.performHealthCheck(ctx)
		}
	}
}

// performHealthCheck performs a single health check against the main pod
func (p *HealthProber) performHealthCheck(ctx context.Context) {
	
	// Create timeout context for this specific health check
	checkCtx, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	
	// Get current target main pod
	targetMainIndex, err := p.controller.GetTargetMainIndex(checkCtx)
	if err != nil {
		common.GetLogger().Error("Health check failed: cannot get target main index", "error", err)
		p.recordFailure(ctx)
		return
	}
	
	targetMainPodName := p.controller.config.GetPodName(targetMainIndex)
	
	// Perform blackbox health check - try to ping the main pod  
	targetMainNode, err := p.controller.getTargetMainNode(checkCtx)
	if err != nil || targetMainNode == nil {
		common.GetLogger().Error("Health check failed: cannot get main node", "pod_name", targetMainPodName, "error", err)
		p.recordFailure(ctx)
		return
	}
	
	err = targetMainNode.Ping(checkCtx)
	if err != nil {
		common.GetLogger().Warn("Health check failed: cannot ping main pod", 
			"pod_name", targetMainPodName,
			"error", err,
			"consecutive_failures", p.consecutiveFailures+1,
			"failure_threshold", p.config.FailureThreshold)
		p.recordFailure(ctx)
		return
	}
	
	// Health check succeeded
	p.recordSuccess(ctx, targetMainPodName)
}

// recordFailure records a health check failure and triggers failover if threshold is reached
func (p *HealthProber) recordFailure(ctx context.Context) {
	logger := common.LoggerFromContext(ctx)
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
		go p.triggerFailover()
		
		// Don't reset the counter - we should only trigger failover once
		// The counter will be reset when the pod recovers (recordSuccess)
	}
}

// recordSuccess records a health check success
func (p *HealthProber) recordSuccess(ctx context.Context, podName string) {
	logger := common.LoggerFromContext(ctx)
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
func (p *HealthProber) triggerFailover() {
	// Guard against nil controller (for testing)
	if p.controller == nil {
		common.GetLogger().Warn("Health prober: cannot trigger failover with nil controller")
		return
	}
	
	// Guard against nil failoverCheckQueue
	if p.controller.failoverCheckQueue == nil || p.controller.failoverCheckQueue.events == nil {
		common.GetLogger().Warn("Health prober: failoverCheckQueue not initialized")
		return
	}
	
	common.GetLogger().Info("Health prober: queueing failover check event")
	
	// Get current target main pod name
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	targetMainIndex, err := p.controller.GetTargetMainIndex(ctx)
	if err != nil {
		common.GetLogger().Error("Health prober: cannot get target main index", "error", err)
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