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

	err := c.executeFailoverCheck(ctx, event.PodName)
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

// executeFailoverCheck implements the failover check logic
func (c *MemgraphController) executeFailoverCheck(ctx context.Context, podName string) error {
	// Step 1: Check if the pod is actually the current target main
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main index: %w", err)
	}

	expectedMainPodName := c.config.GetPodName(targetMainIndex)
	if podName != expectedMainPodName {
		log.Printf("Failover check: Pod %s is not the target main (%s), ignoring", podName, expectedMainPodName)
		return nil
	}

	log.Printf("Failover check: Verifying target main pod %s status", podName)

	// Step 2: Check if targetMain is functioning as main
	// According to DESIGN.md: failover when "pod status of TargetMainPod is not ready"
	// We need to check both Kubernetes pod readiness AND Memgraph functionality

	var mainFunctioning bool

	// First check if pod exists and is ready in Kubernetes (using informer cache)
	pod, err := c.getPodFromCache(podName)
	if err != nil {
		log.Printf("Failover check: Target main pod %s not found in cache: %v", podName, err)
		mainFunctioning = false
	} else if !isPodReady(pod) {
		log.Printf("Failover check: Target main pod %s exists but is not ready", podName)
		mainFunctioning = false
	} else {
		// Pod is ready in Kubernetes, now check if Memgraph is functioning as main
		targetMainNode, err := c.getTargetMainNode(ctx)
		if err != nil {
			log.Printf("Failover check: Cannot get target main node %s from cluster state: %v", podName, err)
			mainFunctioning = false
		} else if targetMainNode.GetBoltAddress() == "" {
			log.Printf("Failover check: Target main pod %s has no bolt address", podName)
			mainFunctioning = false
		} else {
			// Try to query the replication role to confirm it's actually functioning as main
			// QueryReplicationRole now uses auto-commit mode directly
			role, err := c.memgraphClient.QueryReplicationRole(ctx, targetMainNode.GetBoltAddress())
			if err != nil {
				log.Printf("Failover check: Cannot query role from target main pod %s: %v", podName, err)
				mainFunctioning = false
			} else if role.Role != "main" {
				log.Printf("Failover check: Target main pod %s has role '%s', not 'main'", podName, role.Role)
				mainFunctioning = false
			} else {
				log.Printf("Failover check: Target main pod %s is functioning properly as main", podName)
				mainFunctioning = true
			}
		}
	}

	if mainFunctioning {
		log.Printf("Failover check: Target main pod %s is functioning, no failover needed", podName)
		return nil
	}

	// Step 3: Check if targetSyncReplica is ready and has replica role
	targetSyncReplicaNode, err := c.getTargetSyncReplicaNode(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target sync replica node: %w", err)
	}

	// Check if sync replica pod is ready (using informer cache)
	syncReplicaPod, err := c.getPodFromCache(targetSyncReplicaNode.GetName())
	if err != nil {
		return fmt.Errorf("failed to get sync replica pod %s from cache: %w", targetSyncReplicaNode.GetName(), err)
	}

	if !isPodReady(syncReplicaPod) {
		log.Printf("Failover check: Sync replica pod %s is not ready, cannot perform failover", targetSyncReplicaNode.GetName())
		return fmt.Errorf("sync replica pod %s is not ready for failover", targetSyncReplicaNode.GetName())
	}

	// Check if sync replica actually has replica role
	// Use WithRetry version which uses auto-commit mode (required for replication queries)
	role, err := c.memgraphClient.QueryReplicationRole(ctx, targetSyncReplicaNode.GetBoltAddress())
	if err != nil {
		log.Printf("Failover check: Failed to query replication role for sync replica %s: %v", targetSyncReplicaNode.GetName(), err)
		return fmt.Errorf("failed to query sync replica role: %w", err)
	}

	if role.Role != "replica" {
		log.Printf("Failover check: Sync replica %s has role '%s', not 'replica' - cannot failover", targetSyncReplicaNode.GetName(), role.Role)
		return fmt.Errorf("sync replica has role '%s', not 'replica'", role.Role)
	}

	// Step 4: All conditions met - perform failover
	log.Printf("🔄 Failover conditions met: main pod %s down, sync replica %s ready with replica role", podName, targetSyncReplicaNode.GetName())

	// Step 4a: Immediately terminate all gateway connections (DESIGN.md line 159)
	log.Printf("🔌 Terminating all gateway connections for failover")
	c.gatewayServer.DisconnectAll()

	// Determine new target main index (swap to sync replica)
	var newTargetMainIndex int
	if targetMainIndex == 0 {
		newTargetMainIndex = 1
	} else {
		newTargetMainIndex = 0
	}
	log.Printf("🔄 Performing failover: %s (index %d) -> %s (index %d)",
		podName, targetMainIndex, targetSyncReplicaNode.GetName(), newTargetMainIndex)

	// Step 4b: Promote sync replica to main role (DESIGN.md line 162)
	if err := targetSyncReplicaNode.SetToMainRole(ctx); err != nil {
		return fmt.Errorf("failed to set sync replica %s to main role: %w", targetSyncReplicaNode.GetName(), err)
	}

	// Update target main index to trigger failover
	log.Printf("🔄 Updating target main index: %d → %d (reason: failover-from-%s)", targetMainIndex, newTargetMainIndex, podName)
	if err := c.SetTargetMainIndex(ctx, newTargetMainIndex); err != nil {
		return fmt.Errorf("failed to update target main index during failover: %w", err)
	}

	log.Printf("✅ Failover completed: new target main is %s (index %d)",
		c.config.GetPodName(newTargetMainIndex), newTargetMainIndex)

	return nil
}

// waitForFailoverCompletion queues a failover check and waits for it to complete
func (c *MemgraphController) waitForFailoverCompletion(ctx context.Context, podName string, timeout time.Duration) error {
	completionChan := make(chan error, 1)

	// Queue the failover check with completion channel
	c.enqueueFailoverCheckEventWithCompletion("reconcile-triggered", "target-main-unavailable", podName, completionChan)

	// Wait for completion, context cancellation, or timeout
	select {
	case err := <-completionChan:
		if err != nil {
			return fmt.Errorf("failover check failed: %w", err)
		}
		log.Printf("✅ Failover completed successfully for pod %s", podName)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("context cancelled while waiting for failover: %w", ctx.Err())
	case <-time.After(timeout):
		return fmt.Errorf("failover timeout after %v for pod %s", timeout, podName)
	}
}

// stopFailoverCheckQueue stops the failover check queue processor
func (c *MemgraphController) stopFailoverCheckQueue() {
	if c.failoverCheckQueue != nil {
		c.failoverCheckQueue.cancel()
	}
}

// enqueueFailoverCheckEvent adds a failover check event to the queue
func (c *MemgraphController) enqueueFailoverCheckEvent(eventType, reason, podName string) {
	c.enqueueFailoverCheckEventWithCompletion(eventType, reason, podName, nil)
}

// enqueueFailoverCheckEventWithCompletion adds a failover check event with optional completion notification
func (c *MemgraphController) enqueueFailoverCheckEventWithCompletion(eventType, reason, podName string, completionChan chan error) {
	event := FailoverCheckEvent{
		Type:           eventType,
		Reason:         reason,
		PodName:        podName,
		Timestamp:      time.Now(),
		CompletionChan: completionChan,
	}

	// Non-blocking send with overflow protection
	select {
	case c.failoverCheckQueue.events <- event:
		log.Printf("🚨 Enqueued failover check event: %s (pod: %s)", reason, podName)
	default:
		log.Printf("⚠️ Failover check queue full, dropping event: %s", reason)
		// If we have a completion channel, notify of the failure
		if completionChan != nil {
			select {
			case completionChan <- fmt.Errorf("failover queue full"):
			default:
			}
		}
	}
}
