package controller

import (
	"context"
	"fmt"
	"log"
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
			log.Println("Failover check queue processor stopped")
			return
		}
	}
}

// handleFailoverCheckEvent processes a single failover check event with deduplication
func (c *MemgraphController) handleFailoverCheckEvent(event FailoverCheckEvent) {
	// Only leaders process failover events
	if !c.IsLeader() {
		log.Printf("Non-leader ignoring failover check event: %s", event.Reason)
		return
	}

	// Deduplication: ignore events for same pod within 5 seconds (longer than reconcile)
	dedupKey := fmt.Sprintf("failover:%s", event.PodName)

	fq := c.failoverCheckQueue
	fq.dedupMu.Lock()
	lastEventTime, exists := fq.dedup[dedupKey]
	if exists && time.Since(lastEventTime) < 5*time.Second {
		fq.dedupMu.Unlock()
		log.Printf("Deduplicating failover check event: %s (within 5s)", event.Reason)
		return
	}
	fq.dedup[dedupKey] = event.Timestamp
	fq.dedupMu.Unlock()

	// Process the failover check
	log.Printf("🚨 Processing failover check event: %s", event.Reason)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	err := c.performFailoverCheck(ctx)
	if err != nil {
		log.Printf("❌ Failed failover check for event %s: %v", event.Reason, err)
	} else {
		log.Printf("✅ Completed failover check for event: %s", event.Reason)
	}

	// Notify completion if there's a completion channel
	if event.CompletionChan != nil {
		select {
		case event.CompletionChan <- err:
			// Successfully sent completion
		default:
			// Channel might be closed or full, log and continue
			log.Printf("Warning: Could not send completion for failover event: %s", event.Reason)
		}
	}
}

// performFailoverCheck implements the failover check logic
func (c *MemgraphController) performFailoverCheck(ctx context.Context) error {

	// mutex to prevent concurrent failover checks
	c.failoverMu.Lock()
	defer c.failoverMu.Unlock()

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("cannot get target main index: %w", err)
	}
	targetMainPodName := c.config.GetPodName(targetMainIndex)
	role, isHealthy := c.getHealthyRole(ctx, targetMainPodName)

	if isHealthy && role == "main" {
		log.Printf("Failover check: Current target main pod %s is healthy and in 'main' role, no failover needed", targetMainPodName)
		return nil // No failover needed
	}

	log.Printf("🚨 FAILOVER NEEDED: Target main pod %s is unhealthy or not in 'main' role (role: '%s', healthy: %v)",
		targetMainPodName, role, isHealthy)

	targetSyncReplicaIndex := 1 - targetMainIndex // Assuming 2 pods: 0 and 1
	targetSyncReplicaName := c.config.GetPodName(targetSyncReplicaIndex)
	role, isHealthy = c.getHealthyRole(ctx, targetSyncReplicaName)
	if !isHealthy {
		log.Printf("Failover check: Sync replica pod %s is not healthy, cannot perform failover", targetSyncReplicaName)
		return fmt.Errorf("sync replica pod %s is not healthy for failover", targetSyncReplicaName)
	}

	// Target sync replica is healthy, proceed with failover

	if role == "main" {
		log.Printf("Failover: Sync replica pod %s is already main, skipping promotion", targetSyncReplicaName)
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
	log.Printf("Updated target main index to %d", targetSyncReplicaIndex)

	return nil
}

// promoteSyncReplica promotes sync replica to main
func (c *MemgraphController) promoteSyncReplica(ctx context.Context) error {
	log.Println("Executing failover procedure...")

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("cannot get target main index: %w", err)
	}
	targetSyncReplicaIndex := 1 - targetMainIndex // Assuming 2 pods: 0 and 1
	targetSyncReplicaName := c.config.GetPodName(targetSyncReplicaIndex)
	log.Printf("Promoting sync replica pod %s to main...", targetSyncReplicaName)
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

	log.Printf("Successfully promoted pod %s to main", targetSyncReplicaName)

	return nil
}

// getHealthyRole check if node is helthy and get its role
func (c *MemgraphController) getHealthyRole(ctx context.Context, podName string) (string, bool) {

	pod, err := c.getPodFromCache(podName)
	if err != nil {
		log.Printf("getHealthyRole: pod %s does not exist: %v", podName, err)
		return "", false
	}
	if pod.Status.PodIP == "" {
		log.Printf("getHealthyRole: pod %s exists but has no IP", podName)
		return "", false
	}
	if pod.ObjectMeta.DeletionTimestamp != nil {
		log.Printf("getHealthyRole: pod %s is being deleted", podName)
		return "", false
	}
	if !isPodReady(pod) {
		log.Printf("getHealthyRole: pod %s is not ready", podName)
		return "", false
	}
	// Pod is ready in Kubernetes, now check if Memgraph is functioning as main
	node, exists := c.cluster.MemgraphNodes[podName]
	if !exists {
		log.Printf("getHealthyRole: pod %s is not in MemgraphNodes map", podName)
		return "", false
	}
	role, err := node.GetReplicationRole(ctx)
	if err != nil {
		log.Printf("getHealthyRole: Cannot query role from pod %s: %v", podName, err)
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
		log.Printf("🚨 Enqueued failover check event: %s (pod: %s)", reason, podName)
	default:
		log.Printf("⚠️ Failover check queue full, dropping event: %s", reason)
	}

}
