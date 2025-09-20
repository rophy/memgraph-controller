package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"memgraph-controller/internal/common"
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
func (c *MemgraphController) newReconcileQueue(ctx context.Context) *ReconcileQueue {
	ctx, cancel := context.WithCancel(ctx)

	rq := &ReconcileQueue{
		events: make(chan ReconcileEvent, 100), // Buffered channel for burst events
		dedup:  make(map[string]time.Time),
		ctx:    ctx,
		cancel: cancel,
	}

	// Start the queue processor goroutine
	go c.processReconcileQueue(ctx, rq)

	return rq
}

// processReconcileQueue processes events from the reconciliation queue
func (c *MemgraphController) processReconcileQueue(ctx context.Context, rq *ReconcileQueue) {
	ctx, logger := common.WithAttr(ctx, "thread", "reconcileQueue")

	for {
		select {
		case event := <-rq.events:
			c.handleReconcileEvent(ctx, event)
		case <-rq.ctx.Done():
			logger.Debug("reconcile queue processor stopped")
			return
		}
	}
}

// handleReconcileEvent processes a single reconciliation event with deduplication
func (c *MemgraphController) handleReconcileEvent(ctx context.Context, event ReconcileEvent) {
	logger := common.GetLoggerFromContext(ctx)

	// Only leaders process reconciliation events
	if !c.IsLeader() {
		logger.Debug("non-leader ignoring reconcile event", "reason", event.Reason)
		return
	}

	// Deduplication: ignore events for same pod within 2 seconds
	dedupKey := fmt.Sprintf("%s:%s", event.Type, event.PodName)

	rq := c.reconcileQueue
	rq.dedupMu.Lock()
	lastEventTime, exists := rq.dedup[dedupKey]
	if exists && time.Since(lastEventTime) < 2*time.Second {
		rq.dedupMu.Unlock()
		common.GetLogger().Debug("deduplicating reconcile event", "reason", event.Reason, "within", 2*time.Second)
		return
	}
	rq.dedup[dedupKey] = event.Timestamp
	rq.dedupMu.Unlock()

	// Process the event immediately
	logger.Info("processing immediate reconcile event", "reason", event.Reason)

	timeoutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := c.performReconciliationActions(timeoutCtx); err != nil {
		logger.Error("failed immediate reconciliation", "reason", event.Reason, "error", err)
	} else {
		logger.Info("completed immediate reconciliation", "reason", event.Reason)
	}
}

// stopReconcileQueue stops the reconcile queue processor
func (c *MemgraphController) stopReconcileQueue() {
	if c.reconcileQueue != nil {
		c.reconcileQueue.cancel()
	}
}

// enqueueReconcileEvent adds an event to the reconciliation queue for immediate processing
func (c *MemgraphController) enqueueReconcileEvent(ctx context.Context, eventType, reason, podName string) {
	logger := common.GetLoggerFromContext(ctx)

	event := ReconcileEvent{
		Type:      eventType,
		Reason:    reason,
		PodName:   podName,
		Timestamp: time.Now(),
	}

	// Non-blocking send with overflow protection
	select {
	case c.reconcileQueue.events <- event:
		logger.Info("enqueued reconcile event", "reason", reason, "pod", podName)
	default:
		logger.Debug("reconcile queue full, dropping event", "reason", reason)
	}
}
