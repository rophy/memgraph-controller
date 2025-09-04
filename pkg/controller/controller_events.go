package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// setupInformers sets up Kubernetes informers for event-driven reconciliation
func (c *MemgraphController) setupInformers() {
	// Create shared informer factory with label selector filtering
	labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s", c.config.AppName)
	c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		c.clientset,
		time.Second*30, // Resync period
		informers.WithNamespace(c.config.Namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = labelSelector
		}),
	)

	// Set up pod informer - now automatically filtered to memgraph pods only
	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()

	// Add event handlers for pods - no manual filtering needed
	c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onPodAdd,
		UpdateFunc: c.onPodUpdate,
		DeleteFunc: c.onPodDelete,
	})

}

// setupLeaderElectionCallbacks configures the callbacks for leader election
func (c *MemgraphController) setupLeaderElectionCallbacks() {
	c.leaderElection.SetCallbacks(
		func(ctx context.Context) {
			// OnStartedLeading: This controller instance became leader
			c.leaderMu.Lock()
			c.isLeader = true
			c.leaderMu.Unlock()

			log.Println("🎯 Became leader - loading state and starting controller operations")

			// Load state to determine startup phase (BOOTSTRAP vs OPERATIONAL)
			// State loading now handled via GetTargetMainIndex() calls
		},
		func() {
			// OnStoppedLeading: This controller instance lost leadership
			c.leaderMu.Lock()
			c.isLeader = false
			c.leaderMu.Unlock()

			log.Println("⏹️  Lost leadership - stopping operations")
			// Stop reconciliation operations but keep the process running
		},
		func(identity string) {
			// OnNewLeader: Check if leader actually changed
			c.leaderMu.Lock()
			defer c.leaderMu.Unlock()

			// Only log if this is an actual leader change
			if identity != c.lastKnownLeader {
				log.Printf("👑 New leader elected: %s", identity)
				c.lastKnownLeader = identity
			}
		},
	)
}

// onPodAdd handles pod addition events
func (c *MemgraphController) onPodAdd(obj interface{}) {
	pod := obj.(*v1.Pod)
	// No manual filtering needed - informer is already filtered to memgraph pods
	c.enqueueReconcileEvent("pod-add", "pod-added", pod.Name)
}

// onPodUpdate handles pod update events with immediate processing for critical changes
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// No manual filtering needed - informer is already filtered to memgraph pods

	// Check for IP changes and update connection pool
	if oldPod.Status.PodIP != newPod.Status.PodIP {
		if c.memgraphClient != nil && newPod.Status.PodIP != "" {
			c.memgraphClient.connectionPool.UpdatePodIP(newPod.Name, newPod.Status.PodIP)
		}
	}

	// IMMEDIATE ANALYSIS: Check for critical main pod health changes
	if c.IsLeader() {
		// Use current cluster state directly
		lastState := c.cluster
		stateAge := time.Duration(0) // Immediate state, not cached
		// Get current main from target index to check if this is the main pod
		targetMainIndex, err := c.GetTargetMainIndex(context.Background())
		if err == nil {
			currentMain := c.config.GetPodName(targetMainIndex)
			if currentMain == newPod.Name {
				// This is the current main pod - check for immediate health issues
				if c.isPodBecomeUnhealthy(oldPod, newPod) {
					log.Printf("🚨 IMMEDIATE EVENT: Main pod %s became unhealthy, triggering failover check", newPod.Name)

					// Queue failover check event
					c.enqueueFailoverCheckEvent("pod-update", "main-pod-unhealthy", newPod.Name)

					// Still queue regular reconciliation for other updates
				}
			}
		}

		// Log cluster state age for debugging
		if lastState != nil {
			log.Printf("🔍 Event analysis: cluster state age=%v, currentMain from target index=%s, eventPod=%s",
				stateAge, c.config.GetPodName(targetMainIndex), newPod.Name)
		}
	}

	// Fall back to regular reconciliation for non-critical changes
	if !c.shouldReconcile(oldPod, newPod) {
		return // Skip unnecessary reconciliation
	}

	c.enqueueReconcileEvent("pod-update", "pod-state-changed", newPod.Name)
}

// onPodDelete handles pod deletion events
func (c *MemgraphController) onPodDelete(obj interface{}) {
	pod := obj.(*v1.Pod)

	// Invalidate connection for deleted pod through memgraph client connection pool
	if c.memgraphClient != nil {
		c.memgraphClient.connectionPool.InvalidatePodConnection(pod.Name)
	}

	// Check if the deleted pod is the current main - trigger IMMEDIATE failover
	ctx := context.Background()
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	currentMain := ""
	if err != nil {
		log.Printf("Could not get current main from target index: %v", err)
	} else {
		currentMain = c.config.GetPodName(targetMainIndex)
	}

	if currentMain != "" && pod.Name == currentMain {
		log.Printf("🚨 MAIN POD DELETED: %s - triggering failover check", pod.Name)

		// Queue failover check event - only processed by leader
		c.enqueueFailoverCheckEvent("pod-delete", "main-pod-deleted", pod.Name)
	}

	// Still enqueue for reconciliation cleanup
	c.enqueueReconcileEvent("pod-delete", "pod-deleted", pod.Name)
}

// shouldReconcile determines if a pod update should trigger reconciliation
func (c *MemgraphController) shouldReconcile(oldPod, newPod *v1.Pod) bool {
	// Always reconcile if pod readiness changed
	if isPodReady(oldPod) != isPodReady(newPod) {
		return true
	}

	// Always reconcile if pod IP changed
	if oldPod.Status.PodIP != newPod.Status.PodIP {
		return true
	}

	// Reconcile if pod phase changed
	if oldPod.Status.Phase != newPod.Status.Phase {
		return true
	}

	// Reconcile if deletion timestamp changed (pod being deleted)
	oldDeletion := oldPod.ObjectMeta.DeletionTimestamp != nil
	newDeletion := newPod.ObjectMeta.DeletionTimestamp != nil
	if oldDeletion != newDeletion {
		return true
	}

	// Reconcile if node assignment changed (pod migration)
	if oldPod.Spec.NodeName != newPod.Spec.NodeName {
		return true
	}

	// Skip reconciliation for minor metadata changes
	return false
}
