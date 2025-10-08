package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"memgraph-controller/internal/common"
	"memgraph-controller/internal/httpapi"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
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

	// Start probers
	if c.healthProber != nil {
		c.healthProber.Start(ctx)
	}

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
		logger.Warn("Failover check failed", "error", err)
		reconcileErr = err
		if c.promMetrics != nil {
			c.promMetrics.RecordError("reconciliation")
		}
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

	// Update read-only gateway if enabled
	c.updateReadGatewayUpstream(ctx)

	// Run SHOW REPLICA to TargetMainPod
	replicas, err := targetMainNode.GetReplicas(ctx, true)
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

	// Log replication status for monitoring (no longer drop/re-register based on IP changes)
	for replicaName, replicaInfo := range replicaMap {

		podName := replicaInfo.GetPodName()
		replicationHealthy := replicaInfo.IsHealthy()

		if replicationHealthy {
			logger.Debug("Replica has healthy replication status",
				"pod_name", podName,
				"replica_name", replicaName,
				"sync_mode", replicaInfo.SyncMode)
		} else {
			if replicaInfo.ParsedDataInfo.Status == "diverged" {
				logger.Error("☢ Replication has diverged",
					"pod_name", podName,
					"replica_name", replicaName,
					"sync_mode", replicaInfo.SyncMode,
					"behind", replicaInfo.ParsedDataInfo.Behind)
			} else {
				logger.Info("Replica has unhealthy replication status - letting Memgraph handle retries",
					"pod_name", podName,
					"replica_name", replicaName,
					"health_reason", replicaInfo.GetHealthReason(),
					"sync_mode", replicaInfo.SyncMode,
					"behind", replicaInfo.ParsedDataInfo.Behind)
			}

		}
	}

	targetMainPod, err := c.getPodFromCache(targetMainNode.GetName())
	if err != nil {
		logger.Error("Failed to get target main pod", "error", err)
		return err
	}
	isAllPodsReady := isPodReady(targetMainPod)

	// Iterate through all replica nodes and reconcile their states.
	for podName, node := range c.cluster.MemgraphNodes {
		if podName == targetMainNode.GetName() {
			continue // Skip main node
		}

		pod, err := c.getPodFromCache(podName)
		if err != nil || (!isPodReady(pod) && !isPodTerminating(pod)) {
			logger.Info("Replica pod is not ready and not terminating", "pod_name", podName)
			isAllPodsReady = false
			continue // Skip if pod not ready AND not terminating
		}

		// Log if we're processing a terminating pod
		if isPodTerminating(pod) {
			logger.Info("Processing terminating pod for demotion check", "pod_name", podName)
		}

		// All replica nodes should have role "replica"
		role, err := node.GetReplicationRole(ctx, true)
		if err != nil {
			logger.Info("Failed to get role for pod", "pod_name", podName, "error", err)
			continue // Skip if cannot get role
		}
		if role != "replica" {
			isTerminating := isPodTerminating(pod)
			logger.Info("Pod has wrong role, demoting to replica",
				"pod_name", podName,
				"current_role", role,
				"terminating", isTerminating)
			if err := node.SetToReplicaRole(ctx); err != nil {
				logger.Info("Failed to demote pod to replica", "pod_name", podName, "error", err)
				continue // Skip if cannot demote
			}
			logger.Info("Successfully demoted pod to replica",
				"pod_name", podName,
				"was_terminating", isTerminating)
		}

		replicaName := node.GetReplicaName()
		_, exists := replicaMap[replicaName]
		if exists {
			continue // Already registered
		}

		// Skip replica registration for terminating pods to prevent prestop hook deadlocks
		// During pod termination, DNS resolution fails causing registration failures
		if isPodTerminating(pod) {
			logger.Info("Skipping replica registration for terminating pod",
				"pod_name", podName,
				"replica_name", replicaName)
			continue
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
		replicationAddress := node.GetReplicationFQDN(c.config.StatefulSetName, c.config.Namespace)

		// CRITICAL SAFETY CHECK: Prevent dual-main scenarios before registering this replica
		// This prevents data divergence by ensuring a replica NEVER receives replication
		// requests from more than one main node

		// Check 1: Look for any non-target main nodes
		nonTargetMains := []string{}
		for checkPodName, checkNode := range c.cluster.MemgraphNodes {
			if checkPodName == targetMainNode.GetName() {
				continue // Skip the target main
			}

			checkRole, err := checkNode.GetReplicationRole(ctx, false) // Use cached role
			if err != nil {
				logger.Debug("Could not get role for pod during safety check", "pod_name", checkPodName, "error", err)
				continue
			}

			if checkRole == "main" {
				nonTargetMains = append(nonTargetMains, checkPodName)
			}
		}

		// Check 2: Verify pod count matches (detect unreachable pods that might be MAIN)
		k8sPods := c.cluster.getPodsFromCache() // Get all memgraph pods from K8s
		k8sPodCount := len(k8sPods)
		reachablePodCount := len(c.cluster.MemgraphNodes)

		if len(nonTargetMains) > 0 {
			logger.Error("🚨 DUAL-MAIN DETECTED: Skipping registration for this replica to prevent divergence",
				"replica_name", replicaName,
				"target_main", targetMainNode.GetName(),
				"other_mains", nonTargetMains)
			continue // Skip this replica registration, but continue processing other replicas
		}

		if k8sPodCount != reachablePodCount {
			logger.Warn("⚠️ POD COUNT MISMATCH: Skipping registration for this replica for safety",
				"replica_name", replicaName,
				"k8s_pod_count", k8sPodCount,
				"reachable_pod_count", reachablePodCount,
				"target_main", targetMainNode.GetName())
			continue // Skip this replica registration, but continue processing other replicas
		}

		logger.Debug("✅ Safe to register replica: single main confirmed, all pods reachable",
			"replica_name", replicaName,
			"target_main", targetMainNode.GetName(),
			"pod_count", k8sPodCount)

		// FQDN address with default replication port 10000
		err = targetMainNode.RegisterReplica(ctx, replicaName, replicationAddress, syncMode)
		if err == nil {
			logger.Info("Registered replication",
				"replica_name", replicaName,
				"address", replicationAddress,
				"sync_mode", syncMode)
			continue
		}

		var neo4jErr *neo4j.Neo4jError
		// Couldn't register replica replica0. Error: 3
		// Error 3 means diverged data, which is not recoverable.
		if errors.As(err, &neo4jErr) &&
			neo4jErr.Code == "Memgraph.ClientError.MemgraphError.MemgraphError" &&
			strings.Contains(neo4jErr.Msg, "Error: 3") {
			logger.Error("☢ Failed to register replication - diverged data",
				"replica_name", replicaName,
				"address", replicationAddress,
				"sync_mode", syncMode)
			continue
		}
		// Otherwise, drop the replica and try again.
		logger.Warn("Failed to register replication",
			"replica_name", replicaName,
			"address", replicationAddress,
			"sync_mode", syncMode,
			"error", err)
		err = targetMainNode.DropReplica(ctx, replicaName)
		if err != nil {
			logger.Warn("Failed to drop replication",
				"replica_name", replicaName,
				"error", err)
		}
	}

	if isAllPodsReady {
		err = c.reconcileGateway(ctx)
		if err != nil {
			logger.Error("Failed to reconcile gateway", "error", err)
			return err
		}
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

// reconcileGateway sets up the gateway server for the cluster.
func (c *MemgraphController) reconcileGateway(ctx context.Context) error {
	if c.gatewayServer == nil {
		return fmt.Errorf("gateway server is not set")
	}
	targetMainNode, err := c.getTargetMainNode(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main node: %w", err)
	}
	pod, err := c.getPodFromCache(targetMainNode.GetName())
	if err != nil {
		return fmt.Errorf("failed to get target main pod: %w", err)
	}
	if !isPodReady(pod) {
		return fmt.Errorf("target main pod is not ready")
	}
	if pod.ObjectMeta.DeletionTimestamp != nil {
		return fmt.Errorf("target main pod is being deleted")
	}
	boltAddress, err := targetMainNode.GetBoltAddress()
	if err != nil {
		return fmt.Errorf("failed to get bolt address for target main node: %w", err)
	}
	if c.isHealthy(ctx) != nil {
		return fmt.Errorf("cluster is not healthy")
	}
	c.gatewayServer.SetUpstreamAddress(ctx, boltAddress)
	return nil
}
