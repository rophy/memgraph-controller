package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"memgraph-controller/internal/common"
)

// contextKey is a type for context keys to avoid collisions
type contextKey string

// Context keys
const (
	failoverCheckEventKey contextKey = "failover-check-event"
	goroutineKey          contextKey = "goroutine"
)

// FailoverCheckEvent represents an event that triggers failover checking
type FailoverCheckEvent struct {
	Type           string
	Reason         string
	PodName        string
	Timestamp      time.Time
	CompletionChan chan error // Optional: for synchronous waiting
}

// FailoverCheckQueue manages failover check events
type FailoverCheckQueue struct {
	events  chan FailoverCheckEvent
	dedup   map[string]time.Time // Deduplication map
	dedupMu sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
}

// newFailoverCheckQueue creates and starts a new failover check event queue
func (c *MemgraphController) newFailoverCheckQueue(ctx context.Context) *FailoverCheckQueue {
	ctx, cancel := context.WithCancel(ctx)

	fq := &FailoverCheckQueue{
		events: make(chan FailoverCheckEvent, 50), // Smaller buffer - failovers are less frequent
		dedup:  make(map[string]time.Time),
		ctx:    ctx,
		cancel: cancel,
	}

	// Start the queue processor goroutine
	go c.processFailoverCheckQueue(ctx, fq)

	return fq
}

// processFailoverCheckQueue processes events from the failover check queue
func (c *MemgraphController) processFailoverCheckQueue(ctx context.Context, fq *FailoverCheckQueue) {
	ctx, logger := common.WithAttr(ctx, "thread", "failoverCheckQueue")

	for {
		select {
		case event := <-fq.events:
			c.handleFailoverCheckEvent(ctx, event)
		case <-fq.ctx.Done():
			logger.Info("failover check queue processor stopped")
			return
		}
	}
}

// handleFailoverCheckEvent processes a single failover check event with deduplication
func (c *MemgraphController) handleFailoverCheckEvent(ctx context.Context, event FailoverCheckEvent) {
	logger := common.GetLoggerFromContext(ctx)

	// Only leaders process failover events
	if !c.IsLeader() {
		logger.Debug("non-leader ignoring failover check event", "reason", event.Reason)
		return
	}

	// Deduplication: ignore events for same pod within 2 seconds
	dedupKey := fmt.Sprintf("failover:%s", event.PodName)

	fq := c.failoverCheckQueue
	fq.dedupMu.Lock()
	lastEventTime, exists := fq.dedup[dedupKey]
	if exists && time.Since(lastEventTime) < 2*time.Second {
		fq.dedupMu.Unlock()
		logger.Debug("deduplicating failover check event", "reason", event.Reason, "within", 2*time.Second)
		return
	}
	fq.dedup[dedupKey] = event.Timestamp
	fq.dedupMu.Unlock()

	// Process the failover check
	logger.Info("processing failover check event", "reason", event.Reason)

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Pass the event through context
	timeoutCtx = context.WithValue(timeoutCtx, failoverCheckEventKey, event)

	// Set the failover check needed flag.
	// reconciliation will try best to check this flag and stop early.
	c.failoverCheckNeeded.Store(true)

	// Acquire shared mutex before performing failover check
	// This prevents race conditions with reconciliation
	c.operationMu.Lock()
	err := c.performFailoverCheck(timeoutCtx)
	c.failoverCheckNeeded.Store(false)
	c.operationMu.Unlock()
	if err != nil {
		logger.Warn("failed failover check", "reason", event.Reason, "error", err)
	} else {
		logger.Info("completed failover check", "reason", event.Reason)
	}

	// Notify completion if there's a completion channel
	if event.CompletionChan != nil {
		select {
		case event.CompletionChan <- err:
			// Successfully sent completion
		default:
			// Channel might be closed or full, log and continue
			logger.Error("could not send completion for failover event", "reason", event.Reason)
		}
	}
}

// performFailoverCheck implements the failover check logic
func (c *MemgraphController) performFailoverCheck(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
	start := time.Now()
	defer func() {
		logger.Info("performFailoverCheck completed", "duration_ms", float64(time.Since(start).Nanoseconds())/1e6)
	}()

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("cannot get target main index: %w", err)
	}
	targetMainPodName := c.config.GetPodName(targetMainIndex)

	logger.Debug("performFailoverCheck", "target_main_index", targetMainIndex, "target_main_pod_name", targetMainPodName)

	// Check if this failover check was triggered by health check failure
	if eventValue := ctx.Value(failoverCheckEventKey); eventValue != nil {
		if event, ok := eventValue.(FailoverCheckEvent); ok && event.Reason == "health-check-failure" {
			// For health-check-failure events, run a fresh health check to verify failure
			logger.Info("failover check triggered by health failure, verifying with fresh health check", "pod_name", targetMainPodName)

			// Try to ping the pod with a fresh connection
			if node, exists := c.cluster.MemgraphNodes[targetMainPodName]; exists {
				if err := node.Ping(ctx); err == nil {
					logger.Info("failover check: pod recovered, no failover needed", "pod_name", targetMainPodName)
					return nil
				}
				logger.Warn("failover check: health check still failing, proceeding with failover", "pod_name", targetMainPodName, "error", err)
				// Continue with failover - will execute failover below
			}
			logger.Warn("🚨 FAILOVER NEEDED: Target main pod health check failed", "pod_name", targetMainPodName)
		} else {
			// For other event reasons, use the original logic
			role, isHealthy := c.getHealthyRole(ctx, targetMainPodName)

			if isHealthy && role == "main" {
				logger.Info("failover check: current target main pod is healthy and in 'main' role, no failover needed", "pod_name", targetMainPodName)
				return nil // No failover needed
			}
			logger.Warn("🚨 FAILOVER NEEDED: Target main pod is unhealthy or not in 'main' role", "pod_name", targetMainPodName, "role", role, "healthy", isHealthy)
		}
	} else {
		// No event context (direct call), use original logic
		role, isHealthy := c.getHealthyRole(ctx, targetMainPodName)

		if isHealthy && role == "main" {
			logger.Info("failover check: current target main pod is healthy and in 'main' role, no failover needed", "pod_name", targetMainPodName)
			return nil // No failover needed
		}
		logger.Warn("🚨 FAILOVER NEEDED: Target main pod is unhealthy or not in 'main' role", "pod_name", targetMainPodName, "role", role, "healthy", isHealthy)
	}

	// Call internal version since caller should hold operationMu
	err = c.executeFailoverInternal(ctx)
	if err != nil {
		return fmt.Errorf("failed to execute failover: %w", err)
	}

	return nil
}

// executeFailoverInternal executes the failover logic (internal version, caller must hold operationMu)
func (c *MemgraphController) executeFailoverInternal(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		logger.Error("failover: cannot get target main index",
			"error", err,
		)
		return fmt.Errorf("cannot get target main index: %w", err)
	}
	targetSyncReplicaIndex := 1 - targetMainIndex // Assuming 2 pods: 0 and 1
	targetSyncReplicaName := c.config.GetPodName(targetSyncReplicaIndex)
	role, isHealthy := c.getHealthyRole(ctx, targetSyncReplicaName)
	if !isHealthy {
		logger.Error("failover: sync replica pod is not healthy, cannot perform failover",
			"pod_name", targetSyncReplicaName,
		)
		return fmt.Errorf("sync replica pod is not healthy")
	}

	// Latest known replication status must be "healthy" to sync replica.
	targetMainNode, err := c.getTargetMainNode(ctx)
	if err != nil {
		logger.Error("failover: cannot get target main node",
			"error", err,
		)
		return fmt.Errorf("cannot get target main node: %w", err)
	}
	replicas, err := targetMainNode.GetReplicas(ctx)
	if err != nil {
		logger.Error("failover: cannot get replicas from target main node",
			"error", err,
		)
		return fmt.Errorf("cannot get replicas from target main node: %w", err)
	}
	// Get the sync replica node to use its GetReplicaName() method for proper name conversion
	syncReplicaNode, exists := c.cluster.MemgraphNodes[targetSyncReplicaName]
	if !exists {
		logger.Error("failover: sync replica node not found in cluster state",
			"pod_name", targetSyncReplicaName,
		)
		return fmt.Errorf("sync replica node not found in cluster state")
	}
	targetSyncReplicaMemgraphName := syncReplicaNode.GetReplicaName()
	var syncReplica *ReplicaInfo = nil
	for _, replica := range replicas {
		logger.Debug("failover: checking replica",
			"replica_name", replica.Name,
			"target_sync_replica_memgraph_name", targetSyncReplicaMemgraphName,
		)
		if replica.Name == targetSyncReplicaMemgraphName {
			syncReplica = &replica
			break
		}
	}
	if syncReplica == nil {
		logger.Error("failover: sync replica pod not found in cached replicas list, unsafe to perform failover",
			"pod_name", targetSyncReplicaName,
		)
		return fmt.Errorf("%s not found in cached replicas list, unsafe to perform failover", targetSyncReplicaName)
	}
	if syncReplica.SyncMode != "strict_sync" {
		logger.Error("failover: cached replica type is not \"strict_sync\", unsafe to perform failover",
			"pod_name", targetSyncReplicaName,
			"sync_mode", syncReplica.SyncMode,
		)
		return fmt.Errorf("cached replica type is not \"strict_sync\"")
	}
	if syncReplica.ParsedDataInfo == nil {
		logger.Error("failover: cached replica data_info is nil, unsafe to perform failover",
			"pod_name", targetSyncReplicaName,
		)
		return fmt.Errorf("cached replica data_info is nil")
	}
	if syncReplica.ParsedDataInfo.Status != "ready" {
		logger.Error("failover: cached replica status is not \"ready\", unsafe to perform failover",
			"pod_name", targetSyncReplicaName,
			"status", syncReplica.ParsedDataInfo.Status,
			"data_info", syncReplica.DataInfo,
		)
		return fmt.Errorf("cached replica status is not \"ready\"")
	}
	err = syncReplicaNode.Ping(ctx)
	if err != nil {
		logger.Error("failover: sync replica pod is not reachable, unsafe to perform failover", "pod_name", targetSyncReplicaName)
		return fmt.Errorf("sync replica pod is not reachable")
	}

	// Target sync replica is healthy, proceed with failover

	// Disconnect all client connections and stop accepting new ones
	c.gatewayServer.SetUpstreamAddress(ctx, "")
	if c.readGatewayServer != nil {
		c.readGatewayServer.SetUpstreamAddress(ctx, "")
	}

	if role == "main" {
		logger.Warn("failover: sync replica pod is already main, skipping promotion",
			"pod_name", targetSyncReplicaName,
		)
	} else {
		err := c.promoteSyncReplica(ctx)
		if err != nil {
			return err
		}
	}

	// Flip the target main index
	err = c.SetTargetMainIndex(ctx, targetSyncReplicaIndex)
	if err != nil {
		logger.Error("failover: failed to update target main index",
			"error", err,
		)
		return fmt.Errorf("failed to update target main index: %w", err)
	}
	logger.Info("failover: updated target main index",
		"target_main_index", targetSyncReplicaIndex,
	)

	// Set the new upstream address
	if boltAddress, err := syncReplicaNode.GetBoltAddress(); err == nil {
		c.gatewayServer.SetUpstreamAddress(ctx, boltAddress)
		// Update read gateway to use a different replica if available
		c.updateReadGatewayUpstream(ctx)
	} else {
		logger.Error("Failed to get bolt address for new main node", "error", err)
	}

	return nil
}

// promoteSyncReplica promotes sync replica to main
func (c *MemgraphController) promoteSyncReplica(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
	logger.Info("promoting sync replica to main")

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		logger.Error("failover: cannot get target main index",
			"error", err,
		)
		return fmt.Errorf("cannot get target main index: %w", err)
	}
	targetSyncReplicaIndex := 1 - targetMainIndex // Assuming 2 pods: 0 and 1
	targetSyncReplicaName := c.config.GetPodName(targetSyncReplicaIndex)
	logger.Info("promoting sync replica to main", "pod_name", targetSyncReplicaName)

	// Critical: Protect read gateway from accidental write access during role transition
	c.protectReadGatewayBeforeRoleChange(ctx, targetSyncReplicaName)
	targetSyncNode, exists := c.cluster.MemgraphNodes[targetSyncReplicaName]
	if !exists {
		logger.Error("failover: sync replica node not found in cluster map",
			"pod_name", targetSyncReplicaName,
		)
		return fmt.Errorf("sync replica node %s not found in cluster map", targetSyncReplicaName)
	}
	err = targetSyncNode.SetToMainRole(ctx)
	if err != nil {
		logger.Error("failover: failed to promote pod to main",
			"pod_name", targetSyncReplicaName,
			"error", err,
		)
		return fmt.Errorf("failed to promote pod %s to main: %w", targetSyncReplicaName, err)
	}
	role, err := targetSyncNode.GetReplicationRole(ctx)
	if err != nil {
		logger.Error("failover: failed to verify new role of pod",
			"pod_name", targetSyncReplicaName,
			"error", err,
		)
		return fmt.Errorf("failed to verify new role of pod %s: %w", targetSyncReplicaName, err)
	}
	if role != "main" {
		logger.Error("failover: pod promotion to main did not take effect",
			"pod_name", targetSyncReplicaName,
			"role", role,
		)
		return fmt.Errorf("pod %s promotion to main did not take effect, current role: %s", targetSyncReplicaName, role)
	}

	logger.Info("success promoting sync replica to main", "pod_name", targetSyncReplicaName)

	return nil
}

// getHealthyRole check if node is helthy and get its role
func (c *MemgraphController) getHealthyRole(ctx context.Context, podName string) (string, bool) {
	logger := common.GetLoggerFromContext(ctx)
	pod, err := c.getPodFromCache(podName)
	if err != nil {
		logger.Warn("getHealthyRole: pod does not exist", "pod_name", podName, "error", err)
		return "", false
	}
	if pod.Status.PodIP == "" {
		logger.Warn("getHealthyRole: pod %s exists but has no IP", "pod_name", podName)
		return "", false
	}
	if pod.ObjectMeta.DeletionTimestamp != nil {
		logger.Warn("getHealthyRole: pod %s is being deleted", "pod_name", podName)
		return "", false
	}
	if !isPodReady(pod) {
		logger.Warn("getHealthyRole: pod %s is not ready", "pod_name", podName)
		return "", false
	}
	// Pod is ready in Kubernetes, now check if Memgraph is functioning as main
	node, exists := c.cluster.MemgraphNodes[podName]
	if !exists {
		logger.Warn("getHealthyRole: pod %s is not in MemgraphNodes map", "pod_name", podName)
		return "", false
	}
	role, err := node.GetReplicationRole(ctx)
	if err != nil {
		logger.Warn("getHealthyRole: Cannot query role from pod",
			"pod_name", podName,
			"error", err,
		)
		return role, false
	}
	return role, true
}

// protectReadGatewayBeforeRoleChange ensures read gateway doesn't accidentally gain write access
// during replica-to-main role transitions. This prevents the critical security issue where
// read-only clients could suddenly perform writes when their upstream replica gets promoted.
func (c *MemgraphController) protectReadGatewayBeforeRoleChange(ctx context.Context, podNameBeingPromoted string) {
	logger := common.GetLoggerFromContext(ctx)

	// Skip if read gateway is not enabled
	if c.readGatewayServer == nil {
		logger.Debug("read gateway not enabled, skipping role transition protection")
		return
	}

	// Get current read gateway upstream address
	currentUpstream := c.readGatewayServer.GetUpstreamAddress()
	if currentUpstream == "" {
		logger.Debug("read gateway has no upstream set, no protection needed")
		return
	}

	// Get the address of the pod being promoted
	podBeingPromoted, exists := c.cluster.MemgraphNodes[podNameBeingPromoted]
	if !exists {
		logger.Warn("pod being promoted not found in cluster state", "pod_name", podNameBeingPromoted)
		return
	}

	promotedPodAddress, err := podBeingPromoted.GetBoltAddress()
	if err != nil {
		logger.Warn("failed to get bolt address for pod being promoted", "pod_name", podNameBeingPromoted, "error", err)
		return
	}

	// Check if the current read gateway upstream is the pod being promoted to main
	if currentUpstream == promotedPodAddress {
		logger.Warn("🚨 CRITICAL: Read gateway upstream is being promoted to main - disconnecting all read clients to prevent accidental writes",
			"pod_name", podNameBeingPromoted,
			"current_upstream", currentUpstream,
			"action", "disconnect_all_read_connections")

		// Immediately disconnect all read gateway connections
		c.readGatewayServer.DisconnectAll(ctx)

		// Clear the upstream to prevent new connections until we select a new replica
		c.readGatewayServer.SetUpstreamAddress(ctx, "")

		logger.Info("read gateway connections disconnected, upstream cleared",
			"pod_name", podNameBeingPromoted,
			"reason", "role_transition_protection")
	} else {
		logger.Debug("read gateway upstream is not the pod being promoted, no protection needed",
			"pod_being_promoted", promotedPodAddress,
			"current_upstream", currentUpstream)
	}
}

// stopFailoverCheckQueue stops the failover check queue processor
func (c *MemgraphController) stopFailoverCheckQueue() {
	if c.failoverCheckQueue != nil {
		c.failoverCheckQueue.cancel()
	}
}

// enqueueFailoverCheckEvent adds a failover check event to the queue
func (c *MemgraphController) enqueueFailoverCheckEvent(ctx context.Context, eventType, reason, podName string) {
	logger := common.GetLoggerFromContext(ctx)

	event := FailoverCheckEvent{
		Type:      eventType,
		Reason:    reason,
		PodName:   podName,
		Timestamp: time.Now(),
	}

	// Non-blocking send with overflow protection
	select {
	case c.failoverCheckQueue.events <- event:
		logger.Info("queued failover check event", "reason", reason, "pod_name", podName)
	default:
		logger.Debug("failover check queue full, dropping event", "reason", reason)
	}

}
