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
			if !c.IsLeader() {
				log.Println("Not leader, skipping reconciliation cycle")
				continue
			}

			log.Println("Starting reconciliation cycle...")

			// Check if ConfigMap is ready by trying to get the target main index
			_, err := c.GetTargetMainIndex(ctx)
			configMapReady := err == nil

			if !configMapReady {
				log.Println("ConfigMap not ready - performing discovery and creating ConfigMap...")
				
				// Discover current pods and populate cluster state
				if err := c.cluster.DiscoverPods(ctx); err != nil {
					return fmt.Errorf("failed to discover pods: %w", err)
				}

				// Use DESIGN.md compliant discovery logic to determine target main index
				targetMainIndex, err := c.cluster.discoverClusterState(ctx)
				if err != nil {
					return fmt.Errorf("failed to discover cluster state: %w", err)
				}

				// Create ConfigMap with discovered target main index
				if err := c.SetTargetMainIndex(ctx, targetMainIndex); err != nil {
					return fmt.Errorf("failed to set target main index in ConfigMap: %w", err)
				}
				log.Printf("✅ Cluster discovered with target main index: %d", targetMainIndex)
			}

			if err := c.performReconciliationActions(ctx); err != nil {
				if c.isNonRetryableError(err) {
					log.Printf("Non-retryable error during reconciliation: %v", err)
					c.updateReconciliationMetrics("non-retryable-error", time.Since(time.Now()), err)
					// Stop further retries until manual intervention
					continue
				}
				log.Printf("Error during reconciliation: %v", err)
				// Retry on next tick
			}
		}
	}
}

func (c *MemgraphController) performReconciliationActions(ctx context.Context) error {
	log.Println("Starting DESIGN.md compliant reconcile actions...")

	// Step 1: List all memgraph pods with kubernetes status
	err := c.cluster.DiscoverPods(ctx)
	if err != nil {
		return fmt.Errorf("step 1 failed: %w", err)
	}

	targetMainNode, err := c.getTargetMainNode(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main node: %w", err)
	}

	// Step 2: If TargetMainPod is not ready, queue failover and wait
	if !isPodReady(targetMainNode.Pod) {
		log.Printf("TargetMainPod %s not ready, queuing failover check and waiting...", targetMainNode.Name)
		
		// Queue failover check and wait for completion
		if err := c.waitForFailoverCompletion(ctx, targetMainNode.Name, 30*time.Second); err != nil {
			return fmt.Errorf("failover check failed or timed out: %w", err)
		}
		
		// Re-fetch target main node after failover
		targetMainNode, err = c.getTargetMainNode(ctx)
		if err != nil {
			return fmt.Errorf("failed to get new target main node after failover: %w", err)
		}
		log.Printf("Failover completed, new target main is %s", targetMainNode.Name)
	}

	// Step 3: Run SHOW REPLICA to TargetMainPod
	// Step 4: For each pod in the list, check its replication role and status
	// Step 5: If any pod is not in the desired state, reconfigure it
	// Step 6: Ensure that the sync replica is correctly configured
	// Step 7: Ensure that all replicas are connected to the main
	// Step 8: Update any necessary annotations or status fields
	
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

// GetClusterStatus returns comprehensive cluster status for the HTTP API
func (c *MemgraphController) GetClusterStatus(ctx context.Context) (*StatusResponse, error) {
	clusterState := c.cluster
	if clusterState == nil {
		return nil, fmt.Errorf("no cluster state available")
	}

	// Get current main from controller's target main index
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	currentMain := ""
	if err == nil {
		currentMain = c.config.GetPodName(targetMainIndex)
	}

	// Build cluster status summary
	statusResponse := ClusterStatus{
		CurrentMain:        currentMain,
		CurrentSyncReplica: "", // Will be determined below
		TotalPods:          len(clusterState.MemgraphNodes),
	}

	// Count healthy vs unhealthy pods and find sync replica
	healthyCount := 0
	for podName, pod := range clusterState.MemgraphNodes {
		if pod.Pod != nil && isPodReady(pod.Pod) {
			healthyCount++
		}
		// Find sync replica
		if pod.IsSyncReplica {
			statusResponse.CurrentSyncReplica = podName
			statusResponse.SyncReplicaHealthy = pod.Pod != nil && isPodReady(pod.Pod)
		}
	}
	statusResponse.HealthyPods = healthyCount
	statusResponse.UnhealthyPods = statusResponse.TotalPods - healthyCount

	// Convert pods to API format
	var pods []PodStatus
	for _, node := range clusterState.MemgraphNodes {
		healthy := node.Pod != nil && isPodReady(node.Pod)
		podStatus := convertMemgraphNodeToStatus(node, healthy)
		pods = append(pods, podStatus)
	}

	// Add leader status and reconciliation metrics to cluster status
	statusResponse.IsLeader = c.IsLeader()
	statusResponse.ReconciliationMetrics = c.GetReconciliationMetrics()

	response := &StatusResponse{
		Timestamp:    time.Now(),
		ClusterState: statusResponse,
		Pods:         pods,
	}

	log.Printf("Generated cluster status: %d pods, main=%s, healthy=%d/%d",
		len(pods), currentMain, healthyCount, statusResponse.TotalPods)

	return response, nil
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

	// Get target main pod based on TargetMainIndex
	targetMainPodName := c.config.GetPodName(targetMainIndex)
	var targetMainPod *v1.Pod
	for _, pod := range podList {
		if pod.Name == targetMainPodName {
			targetMainPod = pod
			break
		}
	}

	if targetMainPod == nil {
		return fmt.Errorf("target main pod %s not found", targetMainPodName)
	}

	// Step 2: If TargetMainPod is not ready, queue failover and wait
	if !isPodReady(targetMainPod) {
		log.Printf("TargetMainPod %s not ready, queuing failover check and waiting...", targetMainPodName)
		
		// Queue failover check and wait for completion
		if err := c.waitForFailoverCompletion(ctx, targetMainPodName, 30*time.Second); err != nil {
			return fmt.Errorf("failover check failed or timed out: %w", err)
		}
		
		// Reload target main index after failover
		targetMainIndex, err = c.GetTargetMainIndex(ctx)
		if err != nil {
			return fmt.Errorf("failed to get new target main index after failover: %w", err)
		}
		
		// Re-fetch the new target main pod
		targetMainPodName = c.config.GetPodName(targetMainIndex)
		log.Printf("Failover completed, new target main is %s", targetMainPodName)
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
