package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

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

	// Check if ConfigMap is ready (exists and has valid state)
	configMapReady, err := c.isConfigMapReady(ctx)
	if err != nil {
		return fmt.Errorf("failed to check ConfigMap readiness: %w", err)
	}

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

	// Use the ReconcileActions to perform DESIGN.md compliant reconciliation
	reconcileActions := NewReconcileActions(c, c.cluster)
	
	if err := reconcileActions.ExecuteReconcileActions(ctx); err != nil {
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

// isConfigMapReady checks if the controller state ConfigMap exists and is valid
func (c *MemgraphController) isConfigMapReady(ctx context.Context) (bool, error) {
	state, err := c.getStateManager().LoadState(ctx)
	if err != nil {
		// ConfigMap doesn't exist or is invalid
		log.Printf("ConfigMap not ready: %v", err)
		return false, nil
	}

	// Validate that the state contains required information
	if state.TargetMainIndex < 0 || state.TargetMainIndex > 1 {
		log.Printf("ConfigMap contains invalid TargetMainIndex: %d", state.TargetMainIndex)
		return false, nil
	}

	log.Printf("ConfigMap is ready with TargetMainIndex: %d", state.TargetMainIndex)
	return true, nil
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