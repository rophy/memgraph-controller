package controller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	events    chan FailoverCheckEvent
	dedup     map[string]time.Time // Deduplication map
	dedupMu   sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
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
	
	// Step 2: Check if targetMain actually doesn't exist or is not ready
	_, err = c.getTargetMainNode(ctx)
	if err != nil {
		log.Printf("Failover check: Target main pod %s not found in cluster state", podName)
	} else {
		// Pod exists, check if it's ready
		pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err == nil && isPodReady(pod) {
			log.Printf("Failover check: Target main pod %s is still ready, no failover needed", podName)
			return nil
		}
		log.Printf("Failover check: Target main pod %s exists but is not ready", podName)
	}
	
	// Step 3: Check if targetSyncReplica is ready and has replica role
	targetSyncReplicaNode, err := c.getTargetSyncReplicaNode(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target sync replica node: %w", err)
	}
	
	// Check if sync replica pod is ready
	syncReplicaPod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, targetSyncReplicaNode.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get sync replica pod %s: %w", targetSyncReplicaNode.Name, err)
	}
	
	if !isPodReady(syncReplicaPod) {
		log.Printf("Failover check: Sync replica pod %s is not ready, cannot perform failover", targetSyncReplicaNode.Name)
		return fmt.Errorf("sync replica pod %s is not ready for failover", targetSyncReplicaNode.Name)
	}
	
	// Check if sync replica actually has replica role
	role, err := c.memgraphClient.QueryReplicationRole(ctx, targetSyncReplicaNode.BoltAddress)
	if err != nil {
		log.Printf("Failover check: Failed to query replication role for sync replica %s: %v", targetSyncReplicaNode.Name, err)
		return fmt.Errorf("failed to query sync replica role: %w", err)
	}
	
	if role.Role != "replica" {
		log.Printf("Failover check: Sync replica %s has role '%s', not 'replica' - cannot failover", targetSyncReplicaNode.Name, role.Role)
		return fmt.Errorf("sync replica has role '%s', not 'replica'", role.Role)
	}
	
	// Step 4: All conditions met - perform failover
	log.Printf("🔄 Failover conditions met: main pod %s down, sync replica %s ready with replica role", podName, targetSyncReplicaNode.Name)
	
	// Determine new target main index (swap to sync replica)
	var newTargetMainIndex int
	if targetMainIndex == 0 {
		newTargetMainIndex = 1
	} else {
		newTargetMainIndex = 0
	}
	
	log.Printf("🔄 Performing failover: %s (index %d) -> %s (index %d)", 
		podName, targetMainIndex, targetSyncReplicaNode.Name, newTargetMainIndex)
	
	// Update target main index to trigger failover
	if err := c.updateTargetMainIndex(ctx, newTargetMainIndex, fmt.Sprintf("failover-from-%s", podName)); err != nil {
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