package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"memgraph-controller/internal/common"
	"memgraph-controller/internal/httpapi"
)

// Run starts the controller reconciliation loop
// This assumes all components (informers, servers, leader election) have been started
func (c *MemgraphController) Run(ctx context.Context) error {
	// Add goroutine context for reconciliation loop
	ctx = context.WithValue(ctx, goroutineKey, "reconciliation")
	logger := common.GetLogger().WithContext(ctx)
	ctx = common.WithLogger(ctx, logger)
	
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

	common.GetLogger().Info("Starting reconciliation loop", "interval", c.config.ReconcileInterval)

	// Main reconciliation loop - implements DESIGN.md simplified flow
	for {
		select {
		case <-ctx.Done():
			common.GetLogger().Info("Context cancelled, stopping controller...")
			c.stop()
			return ctx.Err()

		case <-ticker.C:
			if !c.IsLeader() {
				common.GetLogger().Info("Not leader, skipping reconciliation cycle")
				continue
			}

			common.GetLogger().Info("Starting reconciliation cycle...")

			// Check if ConfigMap is ready by trying to get the target main index
			_, err := c.GetTargetMainIndex(ctx)
			if err != nil {
				common.GetLogger().Info("ConfigMap not ready - performing discovery...")

				// Use DESIGN.md compliant discovery logic to determine target main index
				targetMainIndex, err := c.cluster.discoverClusterState(ctx)
				if err != nil {
					return fmt.Errorf("failed to discover cluster state: %w", err)
				}
				if targetMainIndex == -1 {
					// -1 without error means cluster is not ready to be discovered
					common.GetLogger().Info("Cluster is not ready to be discovered")
					continue
				}

				// Create ConfigMap with discovered target main index
				if err := c.SetTargetMainIndex(ctx, targetMainIndex); err != nil {
					return fmt.Errorf("failed to set target main index in ConfigMap: %w", err)
				}
				common.GetLogger().Info("✅ Cluster discovered with target main index", "index", targetMainIndex)
			}

			// Enable gateway connections.
			if targetMainNode, err := c.getTargetMainNode(ctx); err == nil {
				if boltAddress, err := targetMainNode.GetBoltAddress(); err == nil {
					c.gatewayServer.SetUpstreamAddress(boltAddress)
				} else {
					common.GetLogger().Error("Failed to get bolt address for main node", "error", err)
				}
			} else {
				common.GetLogger().Error("Failed to get target main node", "error", err)
			}

			if err := c.performReconciliationActions(ctx); err != nil {
				common.GetLogger().Error("Error during reconciliation", "error", err)
				// Retry on next tick
			}
		}
	}
}

func (c *MemgraphController) performReconciliationActions(ctx context.Context) error {
	logger := common.LoggerFromContext(ctx)
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
	mainBoltAddress, err := targetMainNode.GetBoltAddress()
	if err != nil {
		logger.Info("Failed to get bolt address for target main node", "error", err)
		return err
	}
	c.gatewayServer.SetUpstreamAddress(mainBoltAddress)

	// Run SHOW REPLICA to TargetMainPod
	targetMainNode.ClearCachedInfo()
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

	// Get the target sync replica to ensure we handle it properly
	targetSyncReplicaNode, _ := c.getTargetSyncReplicaNode(ctx)
	var targetSyncReplicaName string
	if targetSyncReplicaNode != nil {
		targetSyncReplicaName = targetSyncReplicaNode.GetReplicaName()
	}

	// Iterate through replicaMap and check for issues that require dropping replicas
	for replicaName, replicaInfo := range replicaMap {
		ipAddress := strings.Split(replicaInfo.SocketAddress, ":")[0]
		podName := replicaInfo.GetPodName()
		pod, err := c.getPodFromCache(podName)

		// Check pod availability first
		if err != nil || !isPodReady(pod) {
			logger.Info("Step 4: Skipping replica check - pod missing or not ready", "pod_name", podName)
			continue
		}

		// Ensure pod has a valid IP address
		if pod.Status.PodIP == "" {
			logger.Info("Step 4: Skipping replica check - pod has no IP address", "pod_name", podName)
			continue
		}

		// Check replication health (actual Memgraph replication status)
		replicationHealthy := replicaInfo.IsHealthy()

		// Check IP correctness
		ipCorrect := (ipAddress == pod.Status.PodIP)

		// Apply the logic: only drop when replication is unhealthy AND IP is incorrect
		// AND the pod is ready with a valid IP (so we can re-register)
		if !replicationHealthy && !ipCorrect {
			isSyncReplica := (replicaName == targetSyncReplicaName || replicaInfo.SyncMode == "sync")

			logger.Info("Step 4: Replica has both unhealthy replication and incorrect IP - will drop and re-register",
				"pod_name", podName,
				"replica_name", replicaName,
				"replica_ip", ipAddress,
				"pod_ip", pod.Status.PodIP,
				"replication_healthy", replicationHealthy,
				"health_reason", replicaInfo.GetHealthReason(),
				"sync_mode", replicaInfo.SyncMode)

			// For sync replicas, be more cautious but still fix IP issues
			if isSyncReplica {
				logger.Warn("Step 4: SYNC replica has both replication and IP issues - dropping to fix",
					"pod_name", podName,
					"replica_name", replicaName)
			}

			// Drop the replica that has both issues
			if err := targetMainNode.DropReplica(ctx, replicaName); err != nil {
				logger.Error("Failed to drop replica with replication and IP issues", "pod_name", podName, "error", err)
				continue
			}
			logger.Info("Dropped replica with replication and IP issues", "pod_name", podName)
			delete(replicaMap, replicaName) // Remove from map to allow re-registration

		} else if !replicationHealthy && ipCorrect {
			// Replication unhealthy but IP correct - let Memgraph handle retries
			logger.Info("Step 4: Replica has unhealthy replication but correct IP - trusting Memgraph auto-retry",
				"pod_name", podName,
				"replica_name", replicaName,
				"health_reason", replicaInfo.GetHealthReason(),
				"sync_mode", replicaInfo.SyncMode)

		} else if replicationHealthy && !ipCorrect {
			// This should not happen - healthy replication with wrong IP is contradictory
			logger.Warn("Step 4: Replica reports healthy replication but has incorrect IP - investigating",
				"pod_name", podName,
				"replica_name", replicaName,
				"replica_ip", ipAddress,
				"pod_ip", pod.Status.PodIP,
				"health_reason", replicaInfo.GetHealthReason(),
				"sync_mode", replicaInfo.SyncMode)

		} else {
			// Both replication healthy and IP correct - perfect state
			logger.Debug("Step 4: Replica is healthy with correct IP",
				"pod_name", podName,
				"replica_name", replicaName)
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

	common.GetLogger().Info("Generated cluster status",
		"pod_count", len(pods), "current_main", currentMain, "healthy_pods", healthyCount, "total_pods", statusResponse.TotalPods)

	return response, nil
}
