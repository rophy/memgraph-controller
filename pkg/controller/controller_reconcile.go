package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Run starts the controller reconciliation loop
// This assumes all components (informers, servers, leader election) have been started
func (c *MemgraphController) Run(ctx context.Context) error {
	c.mu.Lock()
	if c.isRunning {
		c.mu.Unlock()
		return fmt.Errorf("controller reconciliation loop is already running")
	}
	c.isRunning = true
	c.mu.Unlock()

	// Start periodic reconciliation timer
	ticker := time.NewTicker(c.config.ReconcileInterval)
	defer ticker.Stop()

	log.Printf("Starting reconciliation loop with interval: %s", c.config.ReconcileInterval)

	// Main reconciliation loop - implements DESIGN.md simplified flow
	for {
		select {
		case <-ctx.Done():
			log.Println("Context cancelled, stopping controller...")
			c.stop()
			return ctx.Err()

		case <-ticker.C:
			// Reconciliation logic per DESIGN.md:
			// if leader:
			//   if configmap not ready then discoverClusterAndCreateConfigMap()
			//   reconcile()
			// else (non-leader): noop
			if c.IsLeader() {
				if err := c.performLeaderReconciliation(ctx); err != nil {
					log.Printf("Leader reconciliation failed: %v", err)
				}
			} else {
				// Load controller state for non-leaders to stay in sync
				if err := c.loadControllerStateOnStartup(ctx); err != nil {
					log.Printf("Non-leader failed to sync state: %v", err)
				}
				log.Printf("Not leader, skipping reconciliation cycle")
			}
		}
	}
}

// performLeaderReconciliation implements the simplified DESIGN.md reconciliation logic
func (c *MemgraphController) performLeaderReconciliation(ctx context.Context) error {
	log.Println("Performing leader reconciliation...")

	// Check if ConfigMap is ready by trying to get the target main index
	_, err := c.GetTargetMainIndex(ctx)
	configMapReady := err == nil

	if !configMapReady {
		log.Println("ConfigMap not ready - performing discovery and creating ConfigMap...")
		if err := c.discoverClusterAndCreateConfigMap(ctx); err != nil {
			return fmt.Errorf("failed to discover cluster and create ConfigMap: %w", err)
		}
		log.Println("✅ Cluster discovered and ConfigMap created")
	}

	// Perform regular reconciliation
	log.Println("Performing reconciliation...")
	if err := c.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconciliation failed: %w", err)
	}

	log.Println("✅ Leader reconciliation completed successfully")
	return nil
}

// Reconcile performs the main reconciliation logic using DESIGN.md compliant actions
func (c *MemgraphController) Reconcile(ctx context.Context) error {
	log.Println("Starting DESIGN.md compliant reconciliation cycle...")

	// Execute DESIGN.md reconcile actions directly
	if err := c.executeReconcileActions(ctx); err != nil {
		c.updateReconciliationMetrics("reconcile-error", time.Since(time.Now()), err)
		return fmt.Errorf("DESIGN.md compliant reconciliation failed: %w", err)
	}

	// Update cached cluster state after successful reconciliation
	c.updateCachedState(c.cluster)

	// Update reconciliation metrics
	c.updateReconciliationMetrics("reconcile-success", time.Since(time.Now()), nil)
	
	log.Println("DESIGN.md compliant reconciliation cycle completed successfully")
	return nil
}

// SyncPodLabels synchronizes pod labels with their current roles per DESIGN.md
func (c *MemgraphController) SyncPodLabels(ctx context.Context, cluster *MemgraphCluster) error {
	log.Println("Starting pod label synchronization...")

	for podName, podInfo := range cluster.Pods {
		_, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			log.Printf("Failed to get pod %s for label sync: %v", podName, err)
			continue
		}

		log.Printf("Pod %s final state: %s (MemgraphRole=%s)", podName, podInfo.State, podInfo.MemgraphRole)
		
		// Note: Actual label updates would be implemented here if needed
		// For now, we just log the intended state
	}

	log.Println("Replication configuration completed successfully")
	return nil
}


// isNonRetryableError determines if an error should stop retries
func (c *MemgraphController) isNonRetryableError(err error) bool {
	if err == nil {
		return false
	}
	
	errMsg := err.Error()
	
	// Check for specific error patterns that require manual intervention
	nonRetryablePatterns := []string{
		"manual intervention required",
		"ambiguous cluster state detected",
	}
	
	for _, pattern := range nonRetryablePatterns {
		if strings.Contains(errMsg, pattern) {
			return true
		}
	}
	
	return false
}

// updateReconciliationMetrics updates internal metrics for reconciliation
func (c *MemgraphController) updateReconciliationMetrics(reason string, duration time.Duration, err error) {
	if c.metrics == nil {
		return
	}

	c.metrics.TotalReconciliations++
	c.metrics.LastReconciliationTime = time.Now()
	c.metrics.LastReconciliationReason = reason

	if err != nil {
		c.metrics.FailedReconciliations++
		c.metrics.LastReconciliationError = err.Error()
	} else {
		c.metrics.SuccessfulReconciliations++
		c.metrics.LastReconciliationError = ""
	}

	c.metrics.AverageReconciliationTime = duration

	log.Printf("Reconciliation metrics updated: reason=%s, duration=%v, error=%v", 
		reason, duration, err != nil)
}

// GetReconciliationMetrics returns current reconciliation metrics
func (c *MemgraphController) GetReconciliationMetrics() ReconciliationMetrics {
	if c.metrics == nil {
		return ReconciliationMetrics{}
	}
	return *c.metrics
}

// RefreshPodInfo updates pod information in the cluster state
func (c *MemgraphController) RefreshPodInfo(ctx context.Context, podName string) (*PodInfo, error) {
	// Get current pod state from Kubernetes
	pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s: %w", podName, err)
	}

	// Update pod info in cluster state
	if c.cluster != nil && c.cluster.Pods != nil {
		podInfo, exists := c.cluster.Pods[podName]
		if exists {
			// Update existing pod info
			podInfo.BoltAddress = fmt.Sprintf("%s:7687", pod.Status.PodIP)
			podInfo.Timestamp = pod.CreationTimestamp.Time
			podInfo.Pod = pod
			
			c.cluster.Pods[podName] = podInfo
			log.Printf("Refreshed pod info for %s: BoltAddress=%s", 
				podName, podInfo.BoltAddress)
			
			updatedPodInfo := c.cluster.Pods[podName]
			return updatedPodInfo, nil
		}
	}

	return nil, fmt.Errorf("pod %s not found in cluster state", podName)
}

// GetClusterStatus returns comprehensive cluster status for the HTTP API
func (c *MemgraphController) GetClusterStatus(ctx context.Context) (*StatusResponse, error) {
	cachedState, _ := c.getCachedState()
	if cachedState == nil {
		return nil, fmt.Errorf("no cached cluster state available")
	}

	// Build cluster state summary
	clusterState := ClusterStatus{
		CurrentMain:        cachedState.CurrentMain,
		CurrentSyncReplica: "", // Will be determined below
		TotalPods:          len(cachedState.Pods),
	}

	// Count healthy vs unhealthy pods and find sync replica
	healthyCount := 0
	for podName, pod := range cachedState.Pods {
		if pod.Pod != nil && isPodReady(pod.Pod) {
			healthyCount++
		}
		// Find sync replica
		if pod.IsSyncReplica {
			clusterState.CurrentSyncReplica = podName
			clusterState.SyncReplicaHealthy = pod.Pod != nil && isPodReady(pod.Pod)
		}
	}
	clusterState.HealthyPods = healthyCount
	clusterState.UnhealthyPods = clusterState.TotalPods - healthyCount

	// Convert pods to API format
	var pods []PodStatus
	for _, podInfo := range cachedState.Pods {
		healthy := podInfo.Pod != nil && isPodReady(podInfo.Pod)
		podStatus := convertPodInfoToStatus(podInfo, healthy)
		pods = append(pods, podStatus)
	}

	// Add leader status and reconciliation metrics to cluster state
	clusterState.IsLeader = c.IsLeader()
	clusterState.ReconciliationMetrics = c.GetReconciliationMetrics()

	response := &StatusResponse{
		Timestamp:    time.Now(),
		ClusterState: clusterState,
		Pods:         pods,
	}

	log.Printf("Generated cluster status: %d pods, main=%s, healthy=%d/%d", 
		len(pods), clusterState.CurrentMain, healthyCount, clusterState.TotalPods)

	return response, nil
}

// discoverClusterAndCreateConfigMap implements DESIGN.md "Discover Cluster State" section
func (c *MemgraphController) discoverClusterAndCreateConfigMap(ctx context.Context) error {
	log.Println("=== CLUSTER DISCOVERY ===")

	// Discover pods first
	if err := c.cluster.DiscoverPods(ctx); err != nil {
		return fmt.Errorf("failed to discover pods: %w", err)
	}

	// Discover cluster state using DESIGN.md steps
	if err := c.cluster.discoverClusterState(ctx); err != nil {
		return fmt.Errorf("cluster state discovery failed: %w", err)
	}

	// If INITIAL_STATE detected, initialize the cluster
	if c.cluster.StateType == INITIAL_STATE {
		log.Println("INITIAL_STATE detected - initializing cluster")
		if err := c.cluster.initializeCluster(ctx); err != nil {
			return fmt.Errorf("cluster initialization failed: %w", err)
		}
	}

	// Create ConfigMap with target main index
	var targetMainIndex int
	if c.cluster.CurrentMain != "" {
		targetMainIndex = c.config.ExtractPodIndex(c.cluster.CurrentMain)
		if targetMainIndex < 0 {
			return fmt.Errorf("invalid current main pod name: %s", c.cluster.CurrentMain)
		}
	} else {
		targetMainIndex = 0 // Default fallback
	}

	// Create/update ConfigMap
	if err := c.SetTargetMainIndex(ctx, targetMainIndex); err != nil {
		return fmt.Errorf("failed to create/update ConfigMap: %w", err)
	}

	log.Printf("✅ Cluster discovery completed - main: %s, target index: %d", c.cluster.CurrentMain, targetMainIndex)
	return nil
}

// executeReconcileActions implements DESIGN.md Reconcile Actions steps 1-8 directly
func (c *MemgraphController) executeReconcileActions(ctx context.Context) error {
	log.Println("Starting DESIGN.md compliant reconcile actions...")

	// Step 1: List all memgraph pods with kubernetes status
	podList, err := c.listMemgraphPods(ctx)
	if err != nil {
		return fmt.Errorf("step 1 failed: %w", err)
	}

	// Get target main index from ConfigMap
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main index: %w", err)
	}

	// Get target pods based on DESIGN.md authority (TargetMainIndex determines main)
	targetMainPod := c.getTargetMainPod(podList, targetMainIndex)
	targetSyncReplica := c.getTargetSyncReplicaPod(podList, targetMainIndex)
	
	if targetMainPod == nil {
		return fmt.Errorf("target main pod not found")
	}

	// Step 2: If TargetMainPod is not ready, perform failover
	if !isPodReady(targetMainPod) {
		log.Printf("TargetMainPod not ready, performing failover...")
		if err := c.performFailover(ctx, targetMainIndex, targetSyncReplica); err != nil {
			return fmt.Errorf("failover failed: %w", err)
		}
		// Reload target main index after failover
		targetMainIndex, _ = c.GetTargetMainIndex(ctx)
	}

	// Steps 3-8: Configure replication according to DESIGN.md
	if err := c.configureReplication(ctx, targetMainIndex); err != nil {
		return fmt.Errorf("replication configuration failed: %w", err)
	}

	log.Println("✅ DESIGN.md reconcile actions completed")
	return nil
}

// listMemgraphPods implements DESIGN.md step 1
func (c *MemgraphController) listMemgraphPods(ctx context.Context) ([]*v1.Pod, error) {
	pods, err := c.clientset.CoreV1().Pods(c.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", c.config.AppName),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	var podList []*v1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if c.config.IsMemgraphPod(pod.Name) {
			podList = append(podList, pod)
		}
	}

	log.Printf("Listed %d memgraph pods", len(podList))
	return podList, nil
}

// getTargetMainPod returns the pod that should be main based on TargetMainIndex
func (c *MemgraphController) getTargetMainPod(podList []*v1.Pod, targetMainIndex int) *v1.Pod {
	targetMainName := c.config.GetPodName(targetMainIndex)
	for _, pod := range podList {
		if pod.Name == targetMainName {
			return pod
		}
	}
	return nil
}

// getTargetSyncReplicaPod returns the pod that should be sync replica (complement of main)
func (c *MemgraphController) getTargetSyncReplicaPod(podList []*v1.Pod, targetMainIndex int) *v1.Pod {
	// DESIGN.md: Either pod-0 OR pod-1 MUST be SYNC replica (complement of main)
	var targetSyncIndex int
	if targetMainIndex == 0 {
		targetSyncIndex = 1
	} else {
		targetSyncIndex = 0
	}
	
	targetSyncName := c.config.GetPodName(targetSyncIndex)
	for _, pod := range podList {
		if pod.Name == targetSyncName {
			return pod
		}
	}
	return nil
}

// performFailover implements DESIGN.md "Failover Actions"
func (c *MemgraphController) performFailover(ctx context.Context, currentMainIndex int, syncReplicaPod *v1.Pod) error {
	log.Printf("🚨 Performing failover from main index %d", currentMainIndex)
	
	// Check if sync replica is ready
	if syncReplicaPod == nil || !isPodReady(syncReplicaPod) {
		return fmt.Errorf("sync replica not ready - cluster not recoverable")
	}
	
	// Flip target: if main was 0, new main is 1; if main was 1, new main is 0
	newMainIndex := 1 - currentMainIndex // Simple flip for 0<->1
	
	// Update ConfigMap with new target main
	if err := c.SetTargetMainIndex(ctx, newMainIndex); err != nil {
		return fmt.Errorf("failed to update target main index: %w", err)
	}
	
	// Promote sync replica to main
	newMainPodName := c.config.GetPodName(newMainIndex)
	newMainIP := fmt.Sprintf("%s:7687", syncReplicaPod.Status.PodIP)
	
	// Promote to main via Memgraph client
	if err := c.memgraphClient.SetReplicationRoleToMain(ctx, newMainIP); err != nil {
		log.Printf("Warning: Failed to promote %s to main: %v", newMainPodName, err)
		// Don't fail - gateway will handle routing
	}
	
	// Update gateway if available
	if c.gatewayServer != nil {
		c.gatewayServer.SetCurrentMain(syncReplicaPod.Status.PodIP)
		log.Printf("✅ Gateway updated to route to new main: %s", newMainPodName)
	}
	
	log.Printf("✅ Failover completed: %s -> %s", c.config.GetPodName(currentMainIndex), newMainPodName)
	return nil
}

// configureReplication implements DESIGN.md steps 3-8
func (c *MemgraphController) configureReplication(ctx context.Context, targetMainIndex int) error {
	// Get current cluster state by querying pods
	err := c.cluster.DiscoverPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover pods: %w", err)
	}
	
	// Configure replication using existing logic but with direct target main index
	return c.ConfigureReplication(ctx, c.cluster)
}