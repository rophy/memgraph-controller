package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
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
			if err := c.loadControllerStateOnStartup(ctx); err != nil {
				log.Printf("Warning: Failed to load startup state: %v", err)
			}
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
	c.enqueuePodEvent("pod-added")
}

// onPodUpdate handles pod update events with immediate processing for critical changes
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// Only process Memgraph pods
	if !c.config.IsMemgraphPod(newPod.Name) {
		return
	}

	// Check for IP changes and update connection pool through cached cluster state
	if oldPod.Status.PodIP != newPod.Status.PodIP {
		cachedState, _ := c.getCachedState()
		if cachedState != nil {
			cachedState.HandlePodIPChange(newPod.Name, oldPod.Status.PodIP, newPod.Status.PodIP)
		}
	}

	// IMMEDIATE ANALYSIS: Check for critical main pod health changes
	if c.IsLeader() {
		lastState, stateAge := c.getCachedState()
		if lastState != nil && lastState.CurrentMain == newPod.Name {
			// This is the current main pod - check for immediate health issues
			if c.isPodBecomeUnhealthy(oldPod, newPod) {
				log.Printf("🚨 IMMEDIATE EVENT: Main pod %s became unhealthy, triggering immediate failover", newPod.Name)
				
				// Trigger immediate failover in background
				go c.handleImmediateFailover(newPod.Name)
				
				// Don't queue regular reconciliation - immediate action taken
				return
			}
		}
		
		// Log cached state age for debugging
		if lastState != nil {
			log.Printf("🔍 Event analysis: cached state age=%v, currentMain=%s, eventPod=%s", 
				time.Since(stateAge), lastState.CurrentMain, newPod.Name)
		}
	}

	// Fall back to regular reconciliation for non-critical changes
	if !c.shouldReconcile(oldPod, newPod) {
		return // Skip unnecessary reconciliation
	}

	c.enqueuePodEvent("pod-state-changed")
}

// onPodDelete handles pod deletion events
func (c *MemgraphController) onPodDelete(obj interface{}) {
	pod := obj.(*v1.Pod)

	// Invalidate connection for deleted pod through cached cluster state
	cachedState, _ := c.getCachedState()
	if cachedState != nil {
		cachedState.InvalidatePodConnection(pod.Name)
	}

	// Check if the deleted pod is the current main - trigger IMMEDIATE failover
	ctx := context.Background()
	currentMain, err := c.getCurrentMainFromState(ctx)
	if err != nil {
		log.Printf("Could not get current main from state: %v", err)
		currentMain = ""
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
	c.enqueuePodEvent("pod-deleted")
}

// onConfigMapAdd handles ConfigMap creation events
func (c *MemgraphController) onConfigMapAdd(obj interface{}) {
	configMap := obj.(*v1.ConfigMap)

	// Only process our controller state ConfigMap
	if configMap.Name != c.getStateManager().ConfigMapName() {
		return
	}

	log.Printf("🔄 ConfigMap added: %s", configMap.Name)

	// Update gateway server phase based on ConfigMap presence
	// If ConfigMap exists, bootstrap should be complete
	if c.gatewayServer != nil && c.gatewayServer.IsBootstrapPhase() {
		log.Println("✅ Gateway transitioned to operational phase (ConfigMap created, bootstrap complete)")
		c.gatewayServer.SetBootstrapPhase(false)
		log.Println("=== GATEWAY OPERATIONAL PHASE: ACCEPTING client connections ===")
	}

	// Update our cached state if we're not the leader (leaders maintain state directly)
	if !c.IsLeader() {
		if err := c.loadControllerStateOnStartup(context.Background()); err != nil {
			log.Printf("Warning: Failed to reload state after ConfigMap creation: %v", err)
		}
	}
}

// onConfigMapUpdate handles ConfigMap update events for distributed state synchronization
func (c *MemgraphController) onConfigMapUpdate(oldObj, newObj interface{}) {
	newConfigMap := newObj.(*v1.ConfigMap)

	// Only process our controller state ConfigMap
	if newConfigMap.Name != c.getStateManager().ConfigMapName() {
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
		currentTargetIndex := c.getTargetMainIndex()
		
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
	if configMap.Name != c.getStateManager().ConfigMapName() {
		return
	}

	log.Printf("⚠️  ConfigMap deleted: %s - will recreate on next reconciliation", configMap.Name)
}

// handleTargetMainChanged handles external changes to target main index
func (c *MemgraphController) handleTargetMainChanged(newTargetMainIndex int) {
	log.Printf("🔄 Handling target main change to index %d", newTargetMainIndex)

	// Immediately update cached state to reflect the change
	cachedState, _ := c.getCachedState()
	if cachedState != nil {
		// Update gateway to point to new main using IP address
		newMainName := c.config.GetPodName(newTargetMainIndex)
		
		if c.gatewayServer != nil {
			// Get IP-based endpoint to avoid DNS refresh timing issues
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			
			newMainIP, err := c.getPodIPEndpoint(ctx, newTargetMainIndex)
			if err != nil {
				log.Printf("❌ Failed to get IP for new main pod %s: %v", newMainName, err)
			} else {
				c.gatewayServer.SetCurrentMain(newMainIP)
				log.Printf("🔄 Gateway updated to route to new main IP: %s:7687", newMainIP)
			}
		}

		// Update cached state
		cachedState.CurrentMain = newMainName
		c.updateCachedState(cachedState)
	}

	// Enqueue reconciliation to fully process the change
	c.enqueuePodEvent("target-main-changed")
	
	// CRITICAL: Trigger immediate reconciliation for main changes
	if c.IsLeader() {
		log.Println("🚨 CRITICAL: Target main changed - performing immediate reconciliation")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			
			if err := c.performLeaderReconciliation(ctx); err != nil {
				log.Printf("❌ Failed immediate reconciliation after main change: %v", err)
			} else {
				log.Println("✅ Immediate reconciliation completed after main change")
			}
		}()
	}
}

// enqueuePodEvent signals the controller that reconciliation should occur
func (c *MemgraphController) enqueuePodEvent(reason string) {
	log.Printf("Pod event detected: %s (reconciliation will occur on next timer cycle)", reason)
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

// getCurrentMainFromState gets the current main pod name from state manager
func (c *MemgraphController) getCurrentMainFromState(ctx context.Context) (string, error) {
	// Handle test cases where stateManager is nil
	if c.getStateManager() == nil {
		return "", fmt.Errorf("no state manager available")
	}

	state, err := c.getStateManager().LoadState(ctx)
	if err != nil {
		return "", err
	}
	return c.config.GetPodName(state.TargetMainIndex), nil
}

// loadControllerStateOnStartup loads persisted state and determines startup phase
func (c *MemgraphController) loadControllerStateOnStartup(ctx context.Context) error {
	exists, err := c.getStateManager().StateExists(ctx)
	if err != nil {
		return fmt.Errorf("failed to check state existence: %w", err)
	}

	if !exists {
		log.Println("No state ConfigMap found - will start in BOOTSTRAP phase")
		return nil // Keep default bootstrap=true
	}

	state, err := c.getStateManager().LoadState(ctx)
	if err != nil {
		log.Printf("Warning: Failed to load state ConfigMap: %v", err)
		log.Println("Will start in BOOTSTRAP phase as fallback")
		return nil // Don't fail startup, just use bootstrap
	}

	// If state exists, assume bootstrap is completed and start in OPERATIONAL phase

	// Transition gateway to operational phase for non-leaders per DESIGN.md
	if c.gatewayServer != nil {
		c.gatewayServer.SetBootstrapPhase(false)
		log.Printf("✅ Gateway transitioned to operational phase (ConfigMap loaded, bootstrap complete)")
	}

	log.Printf("Loaded persisted state - will start in OPERATIONAL phase: targetMainIndex=%d", state.TargetMainIndex)

	return nil
}

// updateTargetMainIndex updates target main index in state manager
func (c *MemgraphController) updateTargetMainIndex(ctx context.Context, newTargetIndex int, reason string) error {
	if c.cluster != nil {
		return c.cluster.updateTargetMainIndex(ctx, newTargetIndex, reason)
	}
	return fmt.Errorf("cluster not initialized")
}

// getPodIdentity extracts the pod identity (index) from a pod name
func (c *MemgraphController) getPodIdentity() string {
	// This is used for leader election identity
	hostname, _ := os.Getenv("HOSTNAME"), "unknown"
	parts := strings.Split(hostname, "-")
	if len(parts) >= 2 {
		// Extract the hash portion for uniqueness
		return strings.Join(parts[len(parts)-2:], "-")
	}
	return hostname
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
	
	if c.gatewayServer != nil {
		// Get IP-based endpoint to avoid DNS refresh timing issues
		newMainIP, err := c.getPodIPEndpoint(ctx, newMainIndex)
		if err != nil {
			log.Printf("❌ Failed to get IP for new main pod %s during failover: %v", newMainName, err)
		} else {
			c.gatewayServer.SetCurrentMain(newMainIP)
			log.Printf("✅ Gateway updated to route to new main IP: %s:7687", newMainIP)
		}
	}

	log.Printf("✅ Immediate failover completed: %s -> %s", deletedPodName, newMainName)
}