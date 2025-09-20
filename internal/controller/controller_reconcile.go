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
	ctx, logger := common.WithAttr(ctx, "thread", "reconciliation")

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
				if boltAddress, err := targetMainNode.GetBoltAddress(); err == nil {
					c.gatewayServer.SetUpstreamAddress(ctx, boltAddress)

					// Update read-only gateway if enabled
					c.updateReadGatewayUpstream(ctx)
				} else {
					logger.Error("Failed to get bolt address for main node", "error", err)
				}
			} else {
				logger.Error("Failed to get target main node", "error", err)
			}

			// Start probers
			if c.healthProber != nil {
				c.healthProber.Start(ctx)
			}

			if err := c.performReconciliationActions(ctx); err != nil {
				logger.Error("Error during reconciliation", "error", err)
				// Retry on next tick
			}
		}
	}
}

// shouldSkipForFailover checks if the reconciliation should be skipped due to failover check needed
func (c *MemgraphController) shouldSkipForFailover(ctx context.Context) bool {
	logger := common.GetLoggerFromContext(ctx)
	if c.failoverCheckNeeded.Load() {
		logger.Info("Failover check needed, skipping reconciliation")
		return true
	}
	return false
}

func (c *MemgraphController) performReconciliationActions(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
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

	if c.shouldSkipForFailover(ctx) {
		return nil // Retry on next tick
	}
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

	// SetUpstreamAddress() reconciles on changes, just pass latest address.
	mainBoltAddress, err := targetMainNode.GetBoltAddress()
	if err != nil {
		logger.Info("Failed to get bolt address for target main node", "error", err)
		return err
	}
	c.gatewayServer.SetUpstreamAddress(ctx, mainBoltAddress)

	// Update read-only gateway if enabled
	c.updateReadGatewayUpstream(ctx)

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

		if c.shouldSkipForFailover(ctx) {
			return nil // Retry on next tick
		}

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
				logger.Warn("Step 4: STRICT_SYNC replica has both replication and IP issues - dropping to fix",
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

		if c.shouldSkipForFailover(ctx) {
			return nil // Retry on next tick
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
			syncMode = "STRICT_SYNC"
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
