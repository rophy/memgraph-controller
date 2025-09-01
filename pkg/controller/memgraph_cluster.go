package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// MemgraphCluster handles all Memgraph cluster-specific operations and represents the cluster state
type MemgraphCluster struct {
	// Cluster data (formerly ClusterState)
	MemgraphNodes map[string]*MemgraphNode

	// Connection management
	connectionPool *ConnectionPool


	// External dependencies
	clientset      kubernetes.Interface
	config         *Config
	memgraphClient *MemgraphClient
}

// NewMemgraphCluster creates a new MemgraphCluster instance
func NewMemgraphCluster(clientset kubernetes.Interface, config *Config, memgraphClient *MemgraphClient) *MemgraphCluster {
	var connectionPool *ConnectionPool
	if memgraphClient != nil {
		connectionPool = memgraphClient.connectionPool
	}
	if connectionPool == nil {
		connectionPool = NewConnectionPool(config)
	}

	cluster := &MemgraphCluster{
		// Initialize cluster data
		MemgraphNodes: make(map[string]*MemgraphNode),

		// Connection management
		connectionPool: connectionPool,


		// External dependencies
		clientset:      clientset,
		config:         config,
		memgraphClient: memgraphClient,
	}

	// Ensure MemgraphClient uses the shared connection pool
	if cluster.memgraphClient != nil {
		cluster.memgraphClient.SetConnectionPool(cluster.connectionPool)
	}

	return cluster
}

// DiscoverPods discovers running pods with the configured app name and updates cluster state
func (mc *MemgraphCluster) DiscoverPods(ctx context.Context) error {
	pods, err := mc.clientset.CoreV1().Pods(mc.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + mc.config.AppName,
	})
	if err != nil {
		return err
	}

	// Clear existing pods and repopulate
	mc.MemgraphNodes = make(map[string]*MemgraphNode)

	for _, pod := range pods.Items {
		// Only process running pods
		if pod.Status.Phase != "Running" {
			log.Printf("Skipping pod %s in phase %s", pod.Name, pod.Status.Phase)
			continue
		}

		// Only process pods with IP assigned
		if pod.Status.PodIP == "" {
			log.Printf("Skipping pod %s without IP address", pod.Name)
			continue
		}

		node := NewMemgraphNode(&pod, mc.memgraphClient)
		mc.MemgraphNodes[pod.Name] = node

		log.Printf("Discovered pod: %s, IP: %s, Timestamp: %s",
			node.Name,
			pod.Status.PodIP,
			node.Timestamp.Format(time.RFC3339))
	}

	return nil
}

// GetPodsByLabel discovers pods matching the given label selector and updates cluster state
func (mc *MemgraphCluster) GetPodsByLabel(ctx context.Context, labelSelector string) error {
	pods, err := mc.clientset.CoreV1().Pods(mc.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return err
	}

	// Clear existing pods and repopulate with filtered results
	mc.MemgraphNodes = make(map[string]*MemgraphNode)

	for _, pod := range pods.Items {
		node := NewMemgraphNode(&pod, mc.memgraphClient)
		mc.MemgraphNodes[pod.Name] = node
	}

	return nil
}

// GetTargetMainPod returns the pod name for the given target main index
func (mc *MemgraphCluster) GetTargetMainPod(targetMainIndex int) string {
	if targetMainIndex < 0 {
		return ""
	}
	return mc.config.GetPodName(targetMainIndex)
}

// GetTargetSyncReplica returns the pod name of the target SYNC replica pod
// Based on DESIGN.md two-pod authority: if main is pod-0, sync is pod-1; if main is pod-1, sync is pod-0
func (mc *MemgraphCluster) GetTargetSyncReplica(targetMainIndex int) string {
	if targetMainIndex < 0 {
		return ""
	}

	// Two-pod authority: pod-0 and pod-1 form a pair
	var syncReplicaIndex int
	if targetMainIndex == 0 {
		syncReplicaIndex = 1 // main=pod-0 → sync=pod-1
	} else if targetMainIndex == 1 {
		syncReplicaIndex = 0 // main=pod-1 → sync=pod-0
	} else {
		// Invalid target main index (should only be 0 or 1)
		return ""
	}

	return mc.config.GetPodName(syncReplicaIndex)
}



// GetMainPods returns a list of pod names that have the MAIN role
func (mc *MemgraphCluster) GetMainPods() []string {
	var mainPods []string
	for podName, node := range mc.MemgraphNodes {
		if node.MemgraphRole == "main" {
			mainPods = append(mainPods, podName)
		}
	}
	return mainPods
}

// GetReplicaPods returns a list of pod names that have the REPLICA role
func (mc *MemgraphCluster) GetReplicaPods() []string {
	var replicaPods []string
	for podName, node := range mc.MemgraphNodes {
		if node.MemgraphRole == "replica" {
			replicaPods = append(replicaPods, podName)
		}
	}
	return replicaPods
}

// LogMainSelectionDecision logs detailed main selection metrics
func (mc *MemgraphCluster) LogMainSelectionDecision(metrics *MainSelectionMetrics) {
	log.Printf("📊 MAIN SELECTION METRICS:")
	log.Printf("  Timestamp: %s", metrics.Timestamp.Format(time.RFC3339))
	log.Printf("  Selected Main: %s", metrics.SelectedMain)
	log.Printf("  Selection Reason: %s", metrics.SelectionReason)
	log.Printf("  Healthy Pods: %d", metrics.HealthyPodsCount)
	log.Printf("  SYNC Replica Available: %t", metrics.SyncReplicaAvailable)
	log.Printf("  Failover Detected: %t", metrics.FailoverDetected)
	log.Printf("  Decision Factors: %v", metrics.DecisionFactors)
}

// InvalidatePodConnection invalidates connection for a specific pod
func (mc *MemgraphCluster) InvalidatePodConnection(podName string) {
	if mc.connectionPool == nil {
		return
	}

	if node, exists := mc.MemgraphNodes[podName]; exists && node.BoltAddress != "" {
		mc.connectionPool.InvalidateConnection(node.BoltAddress)
		log.Printf("Invalidated connection for pod %s (%s)", podName, node.BoltAddress)
	}
}

// HandlePodIPChange handles IP changes for a pod, invalidating old connections
func (mc *MemgraphCluster) HandlePodIPChange(podName, oldIP, newIP string) {
	if mc.connectionPool == nil {
		return
	}

	if oldIP != "" && oldIP != newIP {
		oldBoltAddress := oldIP + ":7687"
		mc.connectionPool.InvalidateConnection(oldBoltAddress)
		log.Printf("Invalidated connection for pod %s: IP changed from %s to %s", podName, oldIP, newIP)
	}

	// Update the pod info with new IP
	if node, exists := mc.MemgraphNodes[podName]; exists {
		newBoltAddress := ""
		if newIP != "" {
			newBoltAddress = newIP + ":7687"
		}
		node.BoltAddress = newBoltAddress
	}
}

// CloseAllConnections closes all connections in the connection pool
func (mc *MemgraphCluster) CloseAllConnections(ctx context.Context) {
	if mc.connectionPool != nil {
		mc.connectionPool.Close(ctx)
	}
}

// discoverClusterState implements DESIGN.md "Discover Cluster State" section (steps 1-4)
func (mc *MemgraphCluster) discoverClusterState(ctx context.Context) (int, error) {
	log.Println("=== Discover Cluster State ===")

	// Step 1: If kubernetes status of either pod-0 or pod-1 is not ready, log warning and stop
	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Node, pod0Exists := mc.MemgraphNodes[pod0Name]
	pod1Node, pod1Exists := mc.MemgraphNodes[pod1Name]

	if !pod0Exists || !pod1Exists || !isPodReady(pod0Node.Pod) || !isPodReady(pod1Node.Pod) {
		return -1, fmt.Errorf("discover Cluster State step 1: pod-0 or pod-1 not ready - cannot proceed with discovery")
	}

	// Query Memgraph roles for both pods
	if err := pod0Node.QueryReplicationRole(ctx); err != nil {
		return -1, fmt.Errorf("failed to query replication role for node %s: %w", pod0Node.Name, err)
	}
	if err := pod1Node.QueryReplicationRole(ctx); err != nil {
		return -1, fmt.Errorf("failed to query replication role for node %s: %w", pod1Node.Name, err)
	}

	// Step 2: If both pod-0 and pod-1 have replication role as `MAIN` and storage shows 0 edge_count, 0 vertex_count
	if pod0Node.MemgraphRole == "main" && pod1Node.MemgraphRole == "main" {
		// Check if both pods have empty storage (0 edges, 0 vertices)
		if err := pod0Node.QueryStorageInfo(ctx); err != nil {
			return -1, fmt.Errorf("failed to query storage info for node %s: %w", pod0Node.Name, err)
		}
		if err := pod1Node.QueryStorageInfo(ctx); err != nil {
			return -1, fmt.Errorf("failed to query storage info for node %s: %w", pod1Node.Name, err)
		}
		if pod0Node.StorageInfo.EdgeCount != 0 || pod0Node.StorageInfo.VertexCount != 0 ||
			pod1Node.StorageInfo.EdgeCount != 0 || pod1Node.StorageInfo.VertexCount != 0 {
			log.Printf("❌ Discover Cluster State step 2: both pod-0 and pod-1 are MAIN with non-empty storage")
			log.Printf("Pod-0 storage: edges=%d, vertices=%d", pod0Node.StorageInfo.EdgeCount, pod0Node.StorageInfo.VertexCount)
			log.Printf("Pod-1 storage: edges=%d, vertices=%d", pod1Node.StorageInfo.EdgeCount, pod1Node.StorageInfo.VertexCount)
			return -1, fmt.Errorf("discover Cluster State step 2: both pod-0 and pod-1 are MAIN with non-empty storage - manual intervention required")
		}

		// Both pods are MAIN with empty storage - safe to initialize cluster
		log.Printf("✅ Discover Cluster State step 2: both pod-0 and pod-1 are MAIN with empty storage")
		targetMainIndex, err := mc.initializeCluster(ctx)
		if err != nil {
			return -1, fmt.Errorf("cluster initialization failed: %w", err)
		}
		return targetMainIndex, nil
	}

	// Step 3: If one pod is MAIN, the other is REPLICA
	if (pod0Node.MemgraphRole == "main" && pod1Node.MemgraphRole == "replica") ||
		(pod0Node.MemgraphRole == "replica" && pod1Node.MemgraphRole == "main") {
		log.Printf("✅ Discover Cluster State step 3: one pod is MAIN, the other is REPLICA")
		var targetMainIndex int
		if pod0Node.MemgraphRole == "main" {
			targetMainIndex = 0
		} else {
			targetMainIndex = 1
		}
		return targetMainIndex, nil
	}

	// Step 4: Otherwise, the cluster is in an unknown state - log error and stop
	log.Printf("❌ Discover Cluster State step 4: cluster in an unknown state - cannot proceed")
	log.Printf("Pod-0 role: %s", pod0Node.MemgraphRole)
	log.Printf("Pod-1 role: %s", pod1Node.MemgraphRole)
	return -1, fmt.Errorf("discover Cluster State step 4: cluster in an unknown state - manual intervention required")

}

// initializeCluster implements DESIGN.md "Initialize Memgraph Cluster" section
func (mc *MemgraphCluster) initializeCluster(ctx context.Context) (int, error) {
	log.Println("=== Initialize Memgraph Cluster ===")
	log.Println("Controller always use pod-0 as MAIN, pod-1 as SYNC REPLICA")

	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Info := mc.MemgraphNodes[pod0Name]
	pod1Info := mc.MemgraphNodes[pod1Name]

	if pod0Info == nil || pod1Info == nil {
		return -1, fmt.Errorf("pod-0 or pod-1 not found for initialization")
	}

	// Step 1: Run command against pod-1 to demote it into replica
	log.Printf("Step 1: Demoting pod-1 (%s) to replica role", pod1Name)
	if err := pod1Info.SetToReplicaRole(ctx); err != nil {
		return -1, fmt.Errorf("step 1 failed - demote pod-1 to replica: %w", err)
	}

	// Step 2: Run command against pod-0 to set up sync replication
	log.Printf("Step 2: Setting up SYNC replication from pod-0 to pod-1")
	replicaName := pod1Info.GetReplicaName()
	replicaAddress := pod1Info.GetReplicationAddress()

	if err := pod0Info.RegisterReplica(ctx, replicaName, replicaAddress, "SYNC"); err != nil {
		return -1, fmt.Errorf("step 2 failed - register SYNC replica: %w", err)
	}

	// Step 3: Run command against pod-0 to verify replication
	log.Printf("Step 3: Verifying replication status")
	if err := mc.verifyReplicationWithRetry(ctx, pod0Info, replicaName); err != nil {
		return -1, fmt.Errorf("step 3 failed - replication verification: %w", err)
	}

	// Update cluster state to reflect initialization (CurrentMain is now tracked via target index)
	pod1Info.IsSyncReplica = true

	log.Printf("✅ Initialize Memgraph Cluster completed: pod-0 is MAIN, pod-1 is SYNC REPLICA")
	return 0, nil
}


// verifyReplicationWithRetry verifies replication status with exponential retry
func (mc *MemgraphCluster) verifyReplicationWithRetry(ctx context.Context, mainPod *MemgraphNode, replicaName string) error {
	maxRetries := 5
	baseDelay := 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		replicasResp, err := mainPod.QueryReplicas(ctx)
		if err != nil {
			if attempt == maxRetries {
				return fmt.Errorf("failed to query replicas after %d attempts: %w", maxRetries, err)
			}
			delay := time.Duration(attempt) * baseDelay
			log.Printf("Replica query failed (attempt %d/%d), retrying in %v: %v", attempt, maxRetries, delay, err)
			time.Sleep(delay)
			continue
		}

		// Find the replica and check if ready
		for _, replica := range replicasResp.Replicas {
			if replica.Name == replicaName {
				if mc.isReplicaReady(replica) {
					log.Printf("✅ Replication verification successful: %s is ready", replicaName)
					return nil
				}
				log.Printf("Replica %s not ready yet: %s", replicaName, replica.DataInfo)
			}
		}

		if attempt < maxRetries {
			delay := time.Duration(attempt) * baseDelay
			log.Printf("Replication not ready (attempt %d/%d), retrying in %v", attempt, maxRetries, delay)
			time.Sleep(delay)
		}
	}

	return fmt.Errorf("replication verification failed after %d attempts", maxRetries)
}

// isReplicaReady checks if replica has ready status in data_info
func (mc *MemgraphCluster) isReplicaReady(replica ReplicaInfo) bool {
	// According to DESIGN.md, we expect: {memgraph: {behind: 0, status: "ready", ts: 0}}
	// For SYNC replicas, empty {} might also be valid
	if replica.DataInfo == "{}" {
		return true // SYNC replicas may show empty data_info when ready
	}

	// Parse data_info to check status
	parsed, err := parseDataInfo(replica.DataInfo)
	if err != nil {
		log.Printf("Failed to parse data_info for replica %s: %v", replica.Name, err)
		return false
	}

	// Check if status is "ready"
	return parsed.Status == "ready"
}
