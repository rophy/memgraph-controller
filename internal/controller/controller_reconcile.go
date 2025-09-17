package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"memgraph-controller/internal/httpapi"
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

	logger.Info("Starting reconciliation loop", "interval", c.config.ReconcileInterval)

	// Main reconciliation loop - implements DESIGN.md simplified flow
	for {
		select {
		case <-ctx.Done():
			logger.Info("Context cancelled, stopping controller...")
			c.stop()
			return ctx.Err()

		case <-ticker.C:
			if !c.IsLeader() {
				logger.Info("Not leader, skipping reconciliation cycle")
				continue
			}

			logger.Info("Starting reconciliation cycle...")

			// Check if ConfigMap is ready by trying to get the target main index
			_, err := c.GetTargetMainIndex(ctx)
			if err != nil {
				logger.Info("ConfigMap not ready - performing discovery...")

				// Use DESIGN.md compliant discovery logic to determine target main index
				targetMainIndex, err := c.cluster.discoverClusterState(ctx)
				if err != nil {
					return fmt.Errorf("failed to discover cluster state: %w", err)
				}
				if targetMainIndex == -1 {
					// -1 without error means cluster is not ready to be discovered
					logger.Info("Cluster is not ready to be discovered")
					continue
				}

				// Create ConfigMap with discovered target main index
				if err := c.SetTargetMainIndex(ctx, targetMainIndex); err != nil {
					return fmt.Errorf("failed to set target main index in ConfigMap: %w", err)
				}
				logger.Info("✅ Cluster discovered with target main index", "index", targetMainIndex)
			}

			// Enable gateway connections.
			if targetMainNode, err := c.getTargetMainNode(ctx); err == nil {
				c.gatewayServer.SetUpstreamAddress(targetMainNode.GetBoltAddress())
			} else {
				logger.Error("Failed to get target main node", "error", err)
			}

			if err := c.performReconciliationActions(ctx); err != nil {
				logger.Error("Error during reconciliation", "error", err)
				// Retry on next tick
			}
		}
	}
}

func (c *MemgraphController) performReconciliationActions(ctx context.Context) error {
	start := time.Now()
	var reconcileErr error
	defer func() {
		duration := time.Since(start)
		logger.Info("performReconciliationActions completed", "duration_ms", float64(duration.Nanoseconds())/1e6)
		
		// Record Prometheus metrics
		if c.promMetrics != nil {
			c.promMetrics.RecordReconciliation(reconcileErr == nil, duration.Seconds())
		}
		
		// Update legacy metrics
		if c.metrics != nil {
			c.metrics.TotalReconciliations++
			if reconcileErr == nil {
				c.metrics.SuccessfulReconciliations++
			} else {
				c.metrics.FailedReconciliations++
				c.metrics.LastReconciliationError = reconcileErr.Error()
			}
			c.metrics.LastReconciliationTime = time.Now()
			c.metrics.AverageReconciliationTime = duration
		}
	}()

	// Ensure only one reconciliation or failover runs at a time
	// This shared mutex prevents race conditions with failover operations
	c.operationMu.Lock()
	defer c.operationMu.Unlock()

	// Skip reconciliation if TargetMainIndex is still not set.
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		logger.Info("Failed to get target main index", "error", err)
		reconcileErr = err
		if c.promMetrics != nil {
			c.promMetrics.RecordError("reconciliation")
		}
		return nil // Retry on next tick
	}
	if targetMainIndex == -1 {
		logger.Info("Target main index is not set, skipping reconciliation")
		return nil // Retry on next tick
	}

	logger.Info("performReconciliationActions started")

	// List all memgraph pods with kubernetes status
	err = c.cluster.Refresh(ctx)
	if err != nil {
		logger.Info("Failed to refresh cluster state", "error", err)
		reconcileErr = err
		if c.promMetrics != nil {
			c.promMetrics.RecordError("reconciliation")
		}
		return nil // Retry on next tick
	}

	// If TargetMainPod is not ready, queue failover and wait
	err = c.performFailoverCheck(ctx)
	if err != nil {
		logger.Info("Failover check failed", "error", err)
		reconcileErr = err
		if c.promMetrics != nil {
			c.promMetrics.RecordError("reconciliation")
		}
		return nil // Retry on next tick
	}
	targetMainNode, err := c.getTargetMainNode(ctx)
	if err != nil {
		logger.Info("Failed to get target main node", "error", err)
		reconcileErr = err
		if c.promMetrics != nil {
			c.promMetrics.RecordError("reconciliation")
		}
		return nil // Retry on next tick
	}

	// SetUpstreamAddress() reconiles on changes, just pass latest address.
	mainBoltAddress := targetMainNode.GetBoltAddress()
	c.gatewayServer.SetUpstreamAddress(mainBoltAddress)

	// Run SHOW REPLICA to TargetMainPod
	// targetMainNode.ClearCachedInfo()
	replicas, err := targetMainNode.GetReplicas(ctx)
	if err != nil {
		logger.Info("Failed to get replicas from main node", "main_node", targetMainNode.GetName(), "error", err)
		return nil // Retry on next tick
	}
	logger.Info("Main pod has registered replicas", "pod", targetMainNode.GetName(), "replica_count", len(replicas))

	// Create a map of replica names for easy lookup
	replicaMap := make(map[string]ReplicaInfo)
	for _, replica := range replicas {
		replicaMap[replica.Name] = replica
	}

	// Iterate through replicaMap and drop unhealthy ones
	for replicaName, replicaInfo := range replicaMap {
		ipAddress := strings.Split(replicaInfo.SocketAddress, ":")[0]
		podName := replicaInfo.GetPodName()
		pod, err := c.getPodFromCache(podName)
		isHealthy := true
		if err != nil || !isPodReady(pod) {
			isHealthy = false
			logger.Info("Step 4: Replica is not healthy (pod missing or not ready)", "pod_name", podName)
		} else if ipAddress != pod.Status.PodIP {
			isHealthy = false
			logger.Info("Step 4: Replica has IP mismatch", "pod_name", podName, "replica_ip", ipAddress, "pod_ip", pod.Status.PodIP)
		}
		if !isHealthy {
			logger.Info("Step 4: Dropping unhealthy or misconfigured replica", "pod_name", podName)
			if err := targetMainNode.DropReplica(ctx, replicaName); err != nil {
				logger.Info("Failed to drop replica", "pod_name", podName, "error", err)
			} else {
				logger.Info("Dropped replica successfully", "pod_name", podName)
				delete(replicaMap, replicaName) // Remove from map after dropping
			}
		}
	}

	// Iterate through all replica nodes and reconcile their states.
	for podName, node := range c.cluster.MemgraphNodes {
		if podName == targetMainNode.GetName() {
			continue // Skip main node
		}

		pod, err := c.getPodFromCache(podName)
		if err != nil || !isPodReady(pod) {
			logger.Info("Replica pod is not ready", "pod_name", podName)
			continue // Skip if pod not ready
		}

		// Clear cached info to force re-query
		node.ClearCachedInfo()

		// All replica nodes should have role "replica"
		role, err := node.GetReplicationRole(ctx)
		if err != nil {
			logger.Info("Failed to get role for pod", "pod_name", podName, "error", err)
			continue // Skip if cannot get role
		}
		if role != "replica" {
			logger.Info("Pod has wrong role, demoting to replica", "pod_name", podName, "current_role", role)
			if err := node.SetToReplicaRole(ctx); err != nil {
				logger.Info("Failed to demote pod to replica", "pod_name", podName, "error", err)
				continue // Skip if cannot demote
			}
		}

		replicaName := node.GetReplicaName()
		_, exists := replicaMap[replicaName]
		if exists {
			continue // Already registered
		}

		// Missing replication - try to register
		syncReplicaNode, err := c.getTargetSyncReplicaNode(ctx)
		if err != nil {
			logger.Info("Failed to get sync replica node", "error", err)
			continue
		}
		syncMode := "ASYNC"
		if node.GetName() == syncReplicaNode.GetName() {
			syncMode = "SYNC"
		}
		ipAddress := node.GetIpAddress()
		// Specifying replication address without port implies port 10000.
		err = targetMainNode.RegisterReplica(ctx, replicaName, ipAddress, syncMode)
		if err != nil {
			logger.Info("Failed to register replication", "replica_name", replicaName, "address", ipAddress, "sync_mode", syncMode)
		}
		logger.Info("Registered replication", "replica_name", replicaName, "address", ipAddress, "sync_mode", syncMode)
	}

	return nil
}

// GetReconciliationMetrics returns current reconciliation metrics
func (c *MemgraphController) GetReconciliationMetrics() httpapi.ReconciliationMetrics {
	if c.metrics == nil {
		return httpapi.ReconciliationMetrics{}
	}
	return httpapi.ReconciliationMetrics{
		TotalReconciliations:      c.metrics.TotalReconciliations,
		SuccessfulReconciliations: c.metrics.SuccessfulReconciliations,
		FailedReconciliations:     c.metrics.FailedReconciliations,
		AverageReconciliationTime: c.metrics.AverageReconciliationTime,
		LastReconciliationTime:    c.metrics.LastReconciliationTime,
		LastReconciliationReason:  c.metrics.LastReconciliationReason,
		LastReconciliationError:   c.metrics.LastReconciliationError,
	}
}

// GetClusterStatus returns comprehensive cluster status for the HTTP API
func (c *MemgraphController) GetClusterStatus(ctx context.Context) (*httpapi.StatusResponse, error) {
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
	statusResponse := httpapi.ClusterStatus{
		CurrentMain:        currentMain,
		CurrentSyncReplica: "", // Will be determined below
		TotalPods:          len(clusterState.MemgraphNodes),
	}

	// Count healthy vs unhealthy pods and find sync replica
	healthyCount := 0
	syncReplicaHealthy := false
	for podName := range clusterState.MemgraphNodes {
		if cachedPod, err := c.getPodFromCache(podName); err == nil && isPodReady(cachedPod) {
			healthyCount++
			// Check if this is the sync replica
			if targetSyncReplica, err := c.getTargetSyncReplicaNode(ctx); err == nil && podName == targetSyncReplica.GetName() {
				syncReplicaHealthy = true
				statusResponse.CurrentSyncReplica = podName
			}
		}
	}
	statusResponse.HealthyPods = healthyCount
	statusResponse.UnhealthyPods = statusResponse.TotalPods - healthyCount
	statusResponse.SyncReplicaHealthy = syncReplicaHealthy
	
	// Update Prometheus cluster state metrics
	if c.promMetrics != nil {
		clusterHealthy := healthyCount == statusResponse.TotalPods && currentMain != ""
		c.promMetrics.UpdateClusterState(clusterHealthy, statusResponse.TotalPods, healthyCount, syncReplicaHealthy)
	}

	// Convert pods to API format
	var pods []httpapi.PodStatus
	for _, node := range clusterState.MemgraphNodes {
		pod, err := c.getPodFromCache(node.GetName())
		healthy := err == nil && isPodReady(pod)
		podStatus := convertMemgraphNodeToStatus(node, healthy, pod)
		pods = append(pods, podStatus)
	}

	// Get replica registrations from the main node
	var replicaRegistrations []httpapi.ReplicaRegistration
	if targetMainIndex, err := c.GetTargetMainIndex(ctx); err == nil {
		targetMainPodName := c.config.GetPodName(targetMainIndex)
		if mainNode, exists := clusterState.MemgraphNodes[targetMainPodName]; exists {
			if replicas, err := mainNode.GetReplicas(ctx); err == nil {
				for _, replica := range replicas {
					replicaRegistrations = append(replicaRegistrations, httpapi.ReplicaRegistration{
						Name:      replica.Name,
						PodName:   replica.GetPodName(),
						Address:   replica.SocketAddress,
						SyncMode:  replica.SyncMode,
						IsHealthy: replica.IsHealthy(),
					})
				}
			}
		}
	}

	// Add leader status and reconciliation metrics to cluster status
	statusResponse.IsLeader = c.IsLeader()
	statusResponse.ReconciliationMetrics = c.GetReconciliationMetrics()
	statusResponse.ReplicaRegistrations = replicaRegistrations

	response := &httpapi.StatusResponse{
		Timestamp:    time.Now(),
		ClusterState: statusResponse,
		Pods:         pods,
	}

	logger.Info("Generated cluster status",
		"pod_count", len(pods), "current_main", currentMain, "healthy_pods", healthyCount, "total_pods", statusResponse.TotalPods)

	return response, nil
}
