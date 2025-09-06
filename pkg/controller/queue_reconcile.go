package controller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// ReconcileEvent represents an event that triggers reconciliation
type ReconcileEvent struct {
	Type      string
	Reason    string
	PodName   string
	Timestamp time.Time
}

// ReconcileQueue manages immediate reconciliation events
type ReconcileQueue struct {
	events  chan ReconcileEvent
	dedup   map[string]time.Time // Deduplication map
	dedupMu sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
}

// newReconcileQueue creates and starts a new reconcile event queue
func (c *MemgraphController) newReconcileQueue() *ReconcileQueue {
	ctx, cancel := context.WithCancel(context.Background())

	rq := &ReconcileQueue{
		events: make(chan ReconcileEvent, 100), // Buffered channel for burst events
		dedup:  make(map[string]time.Time),
		ctx:    ctx,
		cancel: cancel,
	}

	// Start the queue processor goroutine
	go c.processReconcileQueue(rq)

	return rq
}

// processReconcileQueue processes events from the reconciliation queue
func (c *MemgraphController) processReconcileQueue(rq *ReconcileQueue) {
	for {
		select {
		case event := <-rq.events:
			c.handleReconcileEvent(event)
		case <-rq.ctx.Done():
			log.Println("Reconcile queue processor stopped")
			return
		}
	}
}

// handleReconcileEvent processes a single reconciliation event with deduplication
func (c *MemgraphController) handleReconcileEvent(event ReconcileEvent) {
	// Only leaders process reconciliation events
	if !c.IsLeader() {
		log.Printf("Non-leader ignoring reconcile event: %s", event.Reason)
		return
	}

	// Deduplication: ignore events for same pod within 2 seconds
	dedupKey := fmt.Sprintf("%s:%s", event.Type, event.PodName)

	rq := c.reconcileQueue
	rq.dedupMu.Lock()
	lastEventTime, exists := rq.dedup[dedupKey]
	if exists && time.Since(lastEventTime) < 2*time.Second {
		rq.dedupMu.Unlock()
		log.Printf("Deduplicating reconcile event: %s (within 2s)", event.Reason)
		return
	}
	rq.dedup[dedupKey] = event.Timestamp
	rq.dedupMu.Unlock()

	// Process the event immediately
	log.Printf("🔄 Processing immediate reconcile event: %s", event.Reason)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := c.performReconciliationActions(ctx); err != nil {
		log.Printf("❌ Failed immediate reconciliation for event %s: %v", event.Reason, err)
	} else {
		log.Printf("✅ Completed immediate reconciliation for event: %s", event.Reason)
	}
}

// stopReconcileQueue stops the reconcile queue processor
func (c *MemgraphController) stopReconcileQueue() {
	if c.reconcileQueue != nil {
		c.reconcileQueue.cancel()
	}
}

// enqueueReconcileEvent adds an event to the reconciliation queue for immediate processing
func (c *MemgraphController) enqueueReconcileEvent(eventType, reason, podName string) {
	event := ReconcileEvent{
		Type:      eventType,
		Reason:    reason,
		PodName:   podName,
		Timestamp: time.Now(),
	}

	// Non-blocking send with overflow protection
	select {
	case c.reconcileQueue.events <- event:
		log.Printf("📥 Enqueued reconcile event: %s (pod: %s)", reason, podName)
	default:
		log.Printf("⚠️ Reconcile queue full, dropping event: %s", reason)
	}
}
