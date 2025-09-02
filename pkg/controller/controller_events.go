package controller

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// setupInformers sets up Kubernetes informers for event-driven reconciliation
func (c *MemgraphController) setupInformers() {
	// Create shared informer factory
	c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		c.clientset,
		time.Second*30, // Resync period
		informers.WithNamespace(c.config.Namespace),
	)

	// Set up pod informer with label selector
	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()

	// Add event handlers for pods
	c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onPodAdd,
		UpdateFunc: c.onPodUpdate,
		DeleteFunc: c.onPodDelete,
	})

	// Set up ConfigMap informer for controller state synchronization
	c.configMapInformer = c.informerFactory.Core().V1().ConfigMaps().Informer()

	// Add event handlers for ConfigMaps
	c.configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onConfigMapAdd,
		UpdateFunc: c.onConfigMapUpdate,
		DeleteFunc: c.onConfigMapDelete,
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
			// OnNewLeader: A new leader was elected
			log.Printf("👑 New leader elected: %s", identity)
		},
	)
}

// onPodAdd handles pod addition events
func (c *MemgraphController) onPodAdd(obj interface{}) {
	pod := obj.(*v1.Pod)
	if !c.config.IsMemgraphPod(pod.Name) {
		return // Ignore unrelated pods
	}
	c.enqueueReconcileEvent("pod-add", "pod-added", pod.Name)
}

// onPodUpdate handles pod update events with immediate processing for critical changes
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// Only process Memgraph pods
	if !c.config.IsMemgraphPod(newPod.Name) {
		return
	}

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
					log.Printf("🚨 IMMEDIATE EVENT: Main pod %s became unhealthy, triggering immediate failover", newPod.Name)

					// Trigger immediate failover in background
					go c.handleImmediateFailover(newPod.Name)

					// Don't queue regular reconciliation - immediate action taken
					return
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
		log.Printf("🚨 MAIN POD DELETED: %s - triggering IMMEDIATE failover", pod.Name)

		// Only leader should handle failover
		if c.IsLeader() {
			go c.handleImmediateFailover(pod.Name)
		} else {
			log.Printf("Non-leader detected main deletion - leader will handle failover")
		}
	}

	// Still enqueue for reconciliation cleanup
	c.enqueueReconcileEvent("pod-delete", "pod-deleted", pod.Name)
}

// onConfigMapAdd handles ConfigMap creation events
func (c *MemgraphController) onConfigMapAdd(obj interface{}) {
	configMap := obj.(*v1.ConfigMap)

	// Only process our controller state ConfigMap
	if configMap.Name != c.configMapName {
		return
	}

	log.Printf("🔄 ConfigMap added: %s", configMap.Name)

	// Gateway will automatically detect bootstrap phase via bootstrap provider
	// No manual phase transitions needed with dynamic providers

	// Update our cached state if we're not the leader (leaders maintain state directly)
	// State loading now handled via GetTargetMainIndex() calls
}

// onConfigMapUpdate handles ConfigMap update events for distributed state synchronization
func (c *MemgraphController) onConfigMapUpdate(oldObj, newObj interface{}) {
	newConfigMap := newObj.(*v1.ConfigMap)

	// Only process our controller state ConfigMap
	if newConfigMap.Name != c.configMapName {
		return
	}

	log.Printf("🔄 ConfigMap updated: %s", newConfigMap.Name)

	// Parse the new target main index to detect external changes
	targetMainIndexStr, exists := newConfigMap.Data["targetMainIndex"]
	if !exists {
		log.Printf("Warning: ConfigMap %s missing targetMainIndex field", newConfigMap.Name)
		return
	}

	newTargetMainIndex, err := strconv.Atoi(targetMainIndexStr)
	if err != nil {
		log.Printf("Warning: Invalid targetMainIndex in ConfigMap: %s", targetMainIndexStr)
		return
	}

	// Only react to changes if we're the leader
	if c.IsLeader() {
		currentTargetIndex, err := c.GetTargetMainIndex(context.Background())
		if err != nil {
			log.Printf("Failed to get current target main index: %v", err)
			return
		}

		if currentTargetIndex != newTargetMainIndex {
			log.Printf("🔄 Target main changed externally: %d -> %d", currentTargetIndex, newTargetMainIndex)
			c.handleTargetMainChanged(newTargetMainIndex)
		}
	}
}

// onConfigMapDelete handles ConfigMap deletion events
func (c *MemgraphController) onConfigMapDelete(obj interface{}) {
	configMap := obj.(*v1.ConfigMap)

	// Only process our controller state ConfigMap
	if configMap.Name != c.configMapName {
		return
	}

	log.Printf("⚠️  ConfigMap deleted: %s - will recreate on next reconciliation", configMap.Name)
}

// handleTargetMainChanged handles external changes to target main index
func (c *MemgraphController) handleTargetMainChanged(newTargetMainIndex int) {
	log.Printf("🔄 Handling target main change to index %d", newTargetMainIndex)

	// Immediately update cluster state to reflect the change
	if c.cluster != nil {
		// Update gateway to point to new main using IP address
		newMainName := c.config.GetPodName(newTargetMainIndex)

		// Gateway will automatically route to new main via MainNodeProvider
		// No manual endpoint update needed with dynamic providers
		log.Printf("🔄 Gateway will route to new main pod: %s", newMainName)

		// Update cluster state - no need to track CurrentMain anymore since we use GetTargetMainIndex
	}

	// Enqueue reconciliation to fully process the change
	newMainName := c.config.GetPodName(newTargetMainIndex)
	c.enqueueReconcileEvent("target-main-change", "target-main-changed", newMainName)

	// CRITICAL: Trigger immediate reconciliation for main changes
	if c.IsLeader() {
		log.Println("🚨 CRITICAL: Target main changed - performing immediate reconciliation")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			if err := c.executeReconcileActions(ctx); err != nil {
				log.Printf("❌ Failed immediate reconciliation after main change: %v", err)
			} else {
				log.Println("✅ Immediate reconciliation completed after main change")
			}
		}()
	}
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

// updateTargetMainIndex updates target main index
func (c *MemgraphController) updateTargetMainIndex(ctx context.Context, newTargetIndex int, reason string) error {
	currentIndex, _ := c.GetTargetMainIndex(ctx)
	log.Printf("Updating target main index: %d → %d (reason: %s)", currentIndex, newTargetIndex, reason)
	return c.SetTargetMainIndex(ctx, newTargetIndex)
}

// handleImmediateFailover performs immediate failover when main pod fails
func (c *MemgraphController) handleImmediateFailover(deletedPodName string) {
	log.Printf("🚨 Starting immediate failover for deleted main pod: %s", deletedPodName)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Parse pod index to determine next main
	podIndex := c.config.ExtractPodIndex(deletedPodName)
	if podIndex < 0 {
		log.Printf("❌ Could not parse pod index from %s", deletedPodName)
		return
	}

	// Select next available pod as main (pod-1 if pod-0 failed, pod-0 if pod-1 failed)
	var newMainIndex int
	if podIndex == 0 {
		newMainIndex = 1 // Failover to pod-1
	} else {
		newMainIndex = 0 // Failover to pod-0
	}

	log.Printf("🔄 Failing over from pod-%d to pod-%d", podIndex, newMainIndex)

	// Update state to reflect the new main
	err := c.updateTargetMainIndex(ctx, newMainIndex, fmt.Sprintf("immediate-failover-from-%s", deletedPodName))
	if err != nil {
		log.Printf("❌ Failed to update target main index during immediate failover: %v", err)
		return
	}

	// Update gateway immediately to point to new main using IP address
	newMainName := c.config.GetPodName(newMainIndex)

	// Gateway will automatically route to new main via MainNodeProvider
	// No manual endpoint update needed with dynamic providers
	log.Printf("✅ Gateway will route to new main pod: %s", newMainName)

	log.Printf("✅ Immediate failover completed: %s -> %s", deletedPodName, newMainName)
}
