package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
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
			if err != nil {
				log.Println("ConfigMap not ready - performing discovery...")

				// Use DESIGN.md compliant discovery logic to determine target main index
				targetMainIndex, err := c.cluster.discoverClusterState(ctx)
				if err != nil {
					return fmt.Errorf("failed to discover cluster state: %w", err)
				}
				if targetMainIndex == -1 {
					// -1 without error means cluster is not ready to be discovered
					log.Printf("Cluster is not ready to be discovered")
					continue
				}

				// Create ConfigMap with discovered target main index
				if err := c.SetTargetMainIndex(ctx, targetMainIndex); err != nil {
					return fmt.Errorf("failed to set target main index in ConfigMap: %w", err)
				}
				log.Printf("✅ Cluster discovered with target main index: %d", targetMainIndex)
			}

			if err := c.performReconciliationActions(ctx); err != nil {
				log.Printf("Error during reconciliation: %v", err)
				// Retry on next tick
			}
		}
	}
}

func (c *MemgraphController) performReconciliationActions(ctx context.Context) error {
	// Ensure only one reconciliation runs at a time
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()

	// Skip reconciliation if TargetMainIndex is still not set.
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		log.Printf("Failed to get target main index: %v", err)
		return nil // Retry on next tick
	}
	if targetMainIndex == -1 {
		log.Printf("Target main index is not set, skipping reconciliation")
		return nil // Retry on next tick
	}

	log.Println("Starting DESIGN.md compliant reconcile actions...")

	// List all memgraph pods with kubernetes status
	err = c.cluster.Refresh(ctx)
	if err != nil {
		log.Printf("Failed to refresh cluster state: %v", err)
		return nil // Retry on next tick
	}

	// If TargetMainPod is not ready, queue failover and wait
	err = c.performFailoverCheck(ctx)
	if err != nil {
		log.Printf("Failover check failed: %v", err)
		return nil // Retry on next tick
	}
	targetMainNode, err := c.getTargetMainNode(ctx)
	if err != nil {
		log.Printf("Failed to get target main node: %v", err)
		return nil // Retry on next tick
	}

	// Run SHOW REPLICA to TargetMainPod
	replicas, err := targetMainNode.GetReplicas(ctx)
	if err != nil {
		log.Printf("Failed to get replicas from main node %s: %v", targetMainNode.GetName(), err)
		return nil // Retry on next tick
	}
	log.Printf("Main pod %s has %d registered replicas", targetMainNode.GetName(), len(replicas))

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
			log.Printf("Step 4: Replica %s is not healthy (pod missing or not ready)", podName)
		} else if ipAddress != pod.Status.PodIP {
			isHealthy = false
			log.Printf("Step 4: Replica %s has IP %s but pod IP is %s", podName, ipAddress, pod.Status.PodIP)
		}
		if !isHealthy {
			log.Printf("Step 4: Dropping unhealthy or misconfigured replica %s", podName)
			if err := targetMainNode.DropReplica(ctx, replicaName); err != nil {
				log.Printf("Failed to drop replica %s: %v", podName, err)
			} else {
				log.Printf("Dropped replica %s successfully", podName)
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
			log.Printf("Replica pod %s is not ready", podName)
			continue // Skip if pod not ready
		}

		// Clear cached info to force re-query
		node.ClearCachedInfo()

		// All replica nodes should have role "replica"
		role, err := node.GetReplicationRole(ctx)
		if err != nil {
			log.Printf("Failed to get role for pod %s: %v", podName, err)
			continue // Skip if cannot get role
		}
		if role != "replica" {
			log.Printf("Pod %s has role %s but should be replica - demoting...", podName, role)
			if err := node.SetToReplicaRole(ctx); err != nil {
				log.Printf("Failed to demote pod %s to replica: %v", podName, err)
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
			log.Printf("Failed to get sync replica node: %v", err)
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
			log.Printf("Failed to register replication %s for address %s, mode %s", replicaName, ipAddress, syncMode)
		}
		log.Printf("Registered replication %s, address %s, mode %s", replicaName, ipAddress, syncMode)
	}

	return nil
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
	for podName := range clusterState.MemgraphNodes {
		if cachedPod, err := c.getPodFromCache(podName); err == nil && isPodReady(cachedPod) {
			healthyCount++
		}
	}
	statusResponse.HealthyPods = healthyCount
	statusResponse.UnhealthyPods = statusResponse.TotalPods - healthyCount

	// Convert pods to API format
	var pods []PodStatus
	for _, node := range clusterState.MemgraphNodes {
		pod, err := c.getPodFromCache(node.GetName())
		healthy := err == nil && isPodReady(pod)
		podStatus := convertMemgraphNodeToStatus(node, healthy, pod)
		pods = append(pods, podStatus)
	}

	// Get replica registrations from the main node
	var replicaRegistrations []ReplicaRegistration
	if targetMainIndex, err := c.GetTargetMainIndex(ctx); err == nil {
		targetMainPodName := c.config.GetPodName(targetMainIndex)
		if mainNode, exists := clusterState.MemgraphNodes[targetMainPodName]; exists {
			if replicas, err := mainNode.GetReplicas(ctx); err == nil {
				for _, replica := range replicas {
					replicaRegistrations = append(replicaRegistrations, ReplicaRegistration{
						Name:      replica.Name,
						PodName:   replica.GetPodName(),
						IPAddress: strings.Split(replica.SocketAddress, ":")[0],
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

	response := &StatusResponse{
		Timestamp:    time.Now(),
		ClusterState: statusResponse,
		Pods:         pods,
	}

	log.Printf("Generated cluster status: %d pods, main=%s, healthy=%d/%d",
		len(pods), currentMain, healthyCount, statusResponse.TotalPods)

	return response, nil
}
