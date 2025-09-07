package controller

import (
	"context"
	"fmt"
	"sync"
	"time"
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
func (c *MemgraphController) newFailoverCheckQueue() *FailoverCheckQueue {
	ctx, cancel := context.WithCancel(context.Background())

	fq := &FailoverCheckQueue{
		events: make(chan FailoverCheckEvent, 50), // Smaller buffer - failovers are less frequent
		dedup:  make(map[string]time.Time),
		ctx:    ctx,
		cancel: cancel,
	}

	// Start the queue processor goroutine
	go c.processFailoverCheckQueue(fq)

	return fq
}

// processFailoverCheckQueue processes events from the failover check queue
func (c *MemgraphController) processFailoverCheckQueue(fq *FailoverCheckQueue) {
	for {
		select {
		case event := <-fq.events:
			c.handleFailoverCheckEvent(event)
		case <-fq.ctx.Done():
			logger.Info("failover check queue processor stopped")
			return
		}
	}
}

// handleFailoverCheckEvent processes a single failover check event with deduplication
func (c *MemgraphController) handleFailoverCheckEvent(event FailoverCheckEvent) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	err := c.performFailoverCheck(ctx)
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
	start := time.Now()
	defer func() {
		logger.Info("performFailoverCheck completed", "duration_ms", float64(time.Since(start).Nanoseconds())/1e6)
	}()

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("cannot get target main index: %w", err)
	}
	targetMainPodName := c.config.GetPodName(targetMainIndex)
	role, isHealthy := c.getHealthyRole(ctx, targetMainPodName)

	if isHealthy && role == "main" {
		logger.Info("failover check: current target main pod is healthy and in 'main' role, no failover needed", "pod_name", targetMainPodName)
		return nil // No failover needed
	}

	logger.Warn("🚨 FAILOVER NEEDED: Target main pod is unhealthy or not in 'main' role", "pod_name", targetMainPodName, "role", role, "healthy", isHealthy)

	err = c.executeFailover(ctx)
	if err != nil {
		return fmt.Errorf("failed to execute failover: %w", err)
	}

	return nil
}

// executeFailover executes the failover logic
func (c *MemgraphController) executeFailover(ctx context.Context) error {
	// mutex to prevent concurrent failover executions
	c.failoverMu.Lock()
	defer c.failoverMu.Unlock()

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("cannot get target main index: %w", err)
	}
	targetSyncReplicaIndex := 1 - targetMainIndex // Assuming 2 pods: 0 and 1
	targetSyncReplicaName := c.config.GetPodName(targetSyncReplicaIndex)
	role, isHealthy := c.getHealthyRole(ctx, targetSyncReplicaName)
	if !isHealthy {
		logger.Error("failover: sync replica pod is not healthy, cannot perform failover", "pod_name", targetSyncReplicaName)
		return fmt.Errorf("sync replica pod %s is not healthy for failover", targetSyncReplicaName)
	}

	// Latest known replication status must be "healthy" to sync replica.
	targetMainNode, err := c.getTargetMainNode(ctx)
	if err != nil {
		return fmt.Errorf("cannot get target main node: %w", err)
	}
	replicas, err := targetMainNode.GetReplicas(ctx)
	if err != nil {
		return fmt.Errorf("cannot get replicas from target main node: %w", err)
	}
	// Get the sync replica node to use its GetReplicaName() method for proper name conversion
	syncReplicaNode, exists := c.cluster.MemgraphNodes[targetSyncReplicaName]
	if !exists {
		return fmt.Errorf("sync replica node %s not found in cluster state", targetSyncReplicaName)
	}
	targetSyncReplicaMemgraphName := syncReplicaNode.GetReplicaName()
	var syncReplica *ReplicaInfo = nil
	for _, replica := range replicas {
		logger.Debug("failover: checking replica", "replica_name", replica.Name, "target_sync_replica_memgraph_name", targetSyncReplicaMemgraphName)
		if replica.Name == targetSyncReplicaMemgraphName {
			syncReplica = &replica
			break
		}
	}
	if syncReplica == nil {
		logger.Error("failover: sync replica pod not found in cached replicas list, unsafe to perform failover", "pod_name", targetSyncReplicaName)
		return fmt.Errorf("%s not found in cached replicas list, unsafe to perform failover", targetSyncReplicaName)
	}
	if syncReplica.SyncMode != "sync" {
		logger.Error("failover: cached replica type of %s was not \"sync\", unsafe to perform failover", "pod_name", targetSyncReplicaName)
		return fmt.Errorf("cached replica type of %s was not \"sync\", unsafe to perform failover", targetSyncReplicaName)
	}
	if syncReplica.ParsedDataInfo == nil {
		logger.Error("failover: cached replica data_info of %s was nil, unsafe to perform failover", "pod_name", targetSyncReplicaName)
		return fmt.Errorf("cached replica data_info of %s was nil, unsafe to perform failover", targetSyncReplicaName)
	}
	if syncReplica.ParsedDataInfo.Status != "ready" {
		logger.Error("failover: cached replica status of %s was not \"ready\", unsafe to perform failover", "pod_name", targetSyncReplicaName)
		return fmt.Errorf("cached replica status of %s was not \"ready\", unsafe to perform failover", targetSyncReplicaName)
	}
	err = syncReplicaNode.Ping(ctx)
	if err != nil {
		logger.Error("failover: sync replica pod is not reachable, unsafe to perform failover", "pod_name", targetSyncReplicaName)
		return fmt.Errorf("sync replica pod %s is not reachable, unsafe to perform failover", targetSyncReplicaName)
	}

	// Target sync replica is healthy, proceed with failover

	// Disconnect all client connections and stop accepting new ones
	c.gatewayServer.SetUpstreamAddress("")

	if role == "main" {
		logger.Warn("failover: sync replica pod is already main, skipping promotion", "pod_name", targetSyncReplicaName)
	} else {
		err := c.promoteSyncReplica(ctx)
		if err != nil {
			return err
		}
	}

	// Flip the target main index
	err = c.SetTargetMainIndex(ctx, targetSyncReplicaIndex)
	if err != nil {
		return fmt.Errorf("failed to update target main index: %w", err)
	}
	logger.Info("failover check: updated target main index", "target_main_index", targetSyncReplicaIndex)

	// Set the new upstream address
	c.gatewayServer.SetUpstreamAddress(syncReplicaNode.GetBoltAddress())

	return nil
}

// promoteSyncReplica promotes sync replica to main
func (c *MemgraphController) promoteSyncReplica(ctx context.Context) error {
	logger.Info("promoting sync replica to main")

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("cannot get target main index: %w", err)
	}
	targetSyncReplicaIndex := 1 - targetMainIndex // Assuming 2 pods: 0 and 1
	targetSyncReplicaName := c.config.GetPodName(targetSyncReplicaIndex)
	logger.Info("promoting sync replica to main", "pod_name", targetSyncReplicaName)
	targetSyncNode, exists := c.cluster.MemgraphNodes[targetSyncReplicaName]
	if !exists {
		return fmt.Errorf("sync replica node %s not found in cluster map", targetSyncReplicaName)
	}
	err = targetSyncNode.SetToMainRole(ctx)
	if err != nil {
		return fmt.Errorf("failed to promote pod %s to main: %w", targetSyncReplicaName, err)
	}
	role, err := targetSyncNode.GetReplicationRole(ctx)
	if err != nil {
		return fmt.Errorf("failed to verify new role of pod %s: %w", targetSyncReplicaName, err)
	}
	if role != "main" {
		return fmt.Errorf("pod %s promotion to main did not take effect, current role: %s", targetSyncReplicaName, role)
	}

	logger.Info("success promoting sync replica to main", "pod_name", targetSyncReplicaName)

	return nil
}

// getHealthyRole check if node is helthy and get its role
func (c *MemgraphController) getHealthyRole(ctx context.Context, podName string) (string, bool) {

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
		logger.Warn("getHealthyRole: Cannot query role from pod %s", "pod_name", podName, "error", err)
		return role, false
	}
	return role, true
}

// stopFailoverCheckQueue stops the failover check queue processor
func (c *MemgraphController) stopFailoverCheckQueue() {
	if c.failoverCheckQueue != nil {
		c.failoverCheckQueue.cancel()
	}
}

// enqueueFailoverCheckEvent adds a failover check event to the queue
func (c *MemgraphController) enqueueFailoverCheckEvent(eventType, reason, podName string) {
	event := FailoverCheckEvent{
		Type:      eventType,
		Reason:    reason,
		PodName:   podName,
		Timestamp: time.Now(),
	}

	// Non-blocking send with overflow protection
	select {
	case c.failoverCheckQueue.events <- event:
		logger.Debug("queued failover check event", "reason", reason, "pod_name", podName)
	default:
		logger.Debug("failover check queue full, dropping event", "reason", reason)
	}

}
