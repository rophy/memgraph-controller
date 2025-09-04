package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

// MemgraphCluster handles all Memgraph cluster-specific operations and represents the cluster state
type MemgraphCluster struct {
	// Cluster data (formerly ClusterState)
	MemgraphNodes map[string]*MemgraphNode

	// External dependencies
	podCacheStore  cache.Store
	config         *Config
	memgraphClient *MemgraphClient
}

// NewMemgraphCluster creates a new MemgraphCluster instance
func NewMemgraphCluster(podCacheStore cache.Store, config *Config, memgraphClient *MemgraphClient) *MemgraphCluster {

	cluster := &MemgraphCluster{
		// Initialize cluster data
		MemgraphNodes: make(map[string]*MemgraphNode),

		// External dependencies
		podCacheStore:  podCacheStore,
		config:         config,
		memgraphClient: memgraphClient,
	}

	return cluster
}

// DiscoverPods discovers running pods with the configured app name and updates cluster state
func (mc *MemgraphCluster) DiscoverPods(ctx context.Context, getPodList func() []v1.Pod) error {
	// Get pods from informer cache instead of API call
	pods := getPodList()
	
	// Create podList structure similar to API response
	podList := &v1.PodList{
		Items: pods,
	}

	// Create a map of podName to pod for quick lookup
	podMap := make(map[string]v1.Pod)
	for _, pod := range podList.Items {
		podMap[pod.Name] = pod
	}

	// Iterate through current pods in the cluster and disconnect any that no longer exist
	for podName := range mc.MemgraphNodes {
		if _, exists := podMap[podName]; !exists {
			log.Printf("Pod %s no longer exists - disconnecting", podName)
			if err := mc.MemgraphNodes[podName].InvalidateConnection(); err != nil {
				log.Printf("Failed to invalidate connection for pod %s: %v", podName, err)
			}
			// Pod no longer exists - will be handled by MarkPodDeleted if needed
		}
	}

	// Iterate through discovered pods and update or add them to the cluster state
	for podName := range podMap {
		pod := podMap[podName]
		if node, exists := mc.MemgraphNodes[podName]; exists {
			node.UpdatePod(&pod)
		} else {
			// New pod - create MemgraphNode
			log.Printf("Discovered new pod: %s", podName)
			mc.MemgraphNodes[podName] = NewMemgraphNode(&pod, mc.memgraphClient)
		}
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

// discoverClusterState implements DESIGN.md "Discover Cluster State" section (steps 1-4)
// Returns target main index based on current cluster state
func (mc *MemgraphCluster) discoverClusterState(ctx context.Context, getPodFromCache func(string) (*v1.Pod, error)) (int, error) {
	log.Println("=== DISCOVERING CLUSTER STATE ===")

	// Step 1: If kubernetes status of either pod-0 or pod-1 is not ready, log warning and stop
	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Node, pod0Exists := mc.MemgraphNodes[pod0Name]
	pod1Node, pod1Exists := mc.MemgraphNodes[pod1Name]

	// Check if pods are ready using cache
	pod0, pod0CacheErr := getPodFromCache(pod0Name)
	pod1, pod1CacheErr := getPodFromCache(pod1Name)
	
	if !pod0Exists || !pod1Exists || pod0CacheErr != nil || !isPodReady(pod0) || pod1CacheErr != nil || !isPodReady(pod1) {
		log.Printf("DESIGN.md step 1: pod-0 or pod-1 not ready - cannot proceed with discovery")
		return -1, fmt.Errorf("DESIGN.md step 1: pod-0 or pod-1 not ready - cannot proceed with discovery")
	}

	// Query Memgraph roles for both pods
	if err := mc.queryMemgraphRoles(ctx); err != nil {
		return -1, fmt.Errorf("failed to query Memgraph roles: %w", err)
	}

	// Step 2: If both pod-0 and pod-1 have replication role as `MAIN` and storage shows 0 edge_count, 0 vertex_count
	if mc.isBothMainWithEmptyStorage(ctx) {
		log.Println("✅ DESIGN.md step 2: INITIAL_STATE detected (both MAIN, empty storage)")
		// Initialize cluster with pod-0 as main
		if err := mc.initializeCluster(ctx); err != nil {
			return -1, fmt.Errorf("failed to initialize cluster: %w", err)
		}
		return 0, nil // pod-0 becomes main
	}

	// Step 3: If one of pod-0 and pod-1 has replication role as `REPLICA`, the other one as `MAIN`
	if mainPodIndex := mc.getSingleMainPodIndex(); mainPodIndex >= 0 {
		log.Printf("✅ DESIGN.md step 3: OPERATIONAL_STATE detected (main: pod-%d)", mainPodIndex)
		return mainPodIndex, nil
	}

	// Step 4: Otherwise, memgraph-ha is in an unknown state, controller log error and crash immediately
	log.Printf("❌ DESIGN.md step 4: UNKNOWN_STATE detected")
	log.Printf("Pod roles: pod-0=%s, pod-1=%s", pod0Node.MemgraphRole, pod1Node.MemgraphRole)
	return -1, fmt.Errorf("UNKNOWN_STATE: controller must crash - manual intervention required")
}

// initializeCluster implements DESIGN.md "Initialize Memgraph Cluster" section
func (mc *MemgraphCluster) initializeCluster(ctx context.Context) error {
	log.Println("=== INITIALIZING MEMGRAPH CLUSTER ===")
	log.Println("Controller always use pod-0 as MAIN, pod-1 as SYNC REPLICA")

	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Node := mc.MemgraphNodes[pod0Name]
	pod1Node := mc.MemgraphNodes[pod1Name]

	if pod0Node == nil || pod1Node == nil {
		return fmt.Errorf("pod-0 or pod-1 not found for initialization")
	}

	// Step 1: Run command against pod-1 to demote it into replica
	log.Printf("Step 1: Demoting pod-1 (%s) to replica role", pod1Name)
	if err := pod1Node.SetToReplicaRole(ctx); err != nil {
		return fmt.Errorf("step 1 failed - demote pod-1 to replica: %w", err)
	}

	// Step 2: Run command against pod-0 to set up sync replication
	log.Printf("Step 2: Setting up SYNC replication from pod-0 to pod-1")
	pod1ReplicaAddress := fmt.Sprintf("%s:10000", pod1Node.BoltAddress[:strings.LastIndex(pod1Node.BoltAddress, ":")])
	if err := pod0Node.RegisterReplicaWithMode(ctx, pod1Node.ReplicaName, pod1ReplicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("step 2 failed - register SYNC replica: %w", err)
	}

	// Step 3: Run command against pod-0 to verify replication
	log.Printf("Step 3: Verifying replication status")
	replicasResponse, err := pod0Node.QueryReplicas(ctx)
	if err != nil {
		return fmt.Errorf("step 3 failed - query replicas: %w", err)
	}

	// Check if replica shows as ready
	found := false
	for _, replica := range replicasResponse.Replicas {
		if replica.Name == pod1Node.ReplicaName && replica.SyncMode == "SYNC" {
			found = true
			// Parse data_info to check if replica is ready
			if replica.ParsedDataInfo != nil && replica.ParsedDataInfo.Status == "ready" && replica.ParsedDataInfo.Behind == 0 {
				log.Printf("✅ SYNC replica %s is ready and up-to-date", replica.Name)
			} else {
				return fmt.Errorf("SYNC replica %s is not ready: data_info=%s", replica.Name, replica.DataInfo)
			}
			break
		}
	}
	if !found {
		return fmt.Errorf("SYNC replica %s not found in SHOW REPLICAS output", pod1Node.ReplicaName)
	}

	log.Printf("✅ Initialize Memgraph Cluster completed: pod-0 is MAIN, pod-1 is SYNC REPLICA")
	return nil
}

// queryMemgraphRoles queries replication roles from both pods
func (mc *MemgraphCluster) queryMemgraphRoles(ctx context.Context) error {
	for podName, podNode := range mc.MemgraphNodes {
		if !mc.config.IsMemgraphPod(podName) {
			continue
		}

		if podNode.BoltAddress == "" {
			log.Printf("Skipping role query for %s: no bolt address", podName)
			continue
		}

		if err := podNode.QueryReplicationRole(ctx); err != nil {
			log.Printf("Failed to query role for %s: %v", podName, err)
			continue
		}

		log.Printf("Pod %s has Memgraph role: %s", podName, podNode.MemgraphRole)
	}

	return nil
}

// isBothMainWithEmptyStorage checks DESIGN.md step 2 condition
func (mc *MemgraphCluster) isBothMainWithEmptyStorage(ctx context.Context) bool {
	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Node := mc.MemgraphNodes[pod0Name]
	pod1Node := mc.MemgraphNodes[pod1Name]

	// Both must have MAIN role
	if pod0Node.MemgraphRole != "main" || pod1Node.MemgraphRole != "main" {
		return false
	}

	// Both must have empty storage
	return mc.hasEmptyStorage(ctx, pod0Node) && mc.hasEmptyStorage(ctx, pod1Node)
}

// getSingleMainPodIndex checks DESIGN.md step 3 condition - returns main pod index if exactly one main found
func (mc *MemgraphCluster) getSingleMainPodIndex() int {
	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Node := mc.MemgraphNodes[pod0Name]
	pod1Node := mc.MemgraphNodes[pod1Name]

	if pod0Node == nil || pod1Node == nil {
		return -1
	}

	// Check if exactly one is main and one is replica
	if pod0Node.MemgraphRole == "main" && pod1Node.MemgraphRole == "replica" {
		return 0
	}
	if pod1Node.MemgraphRole == "main" && pod0Node.MemgraphRole == "replica" {
		return 1
	}

	return -1
}

// hasEmptyStorage checks if pod has empty storage (0 edges, 0 vertices)
func (mc *MemgraphCluster) hasEmptyStorage(ctx context.Context, podNode *MemgraphNode) bool {
	if podNode.BoltAddress == "" {
		log.Printf("Cannot check storage for pod %s: no bolt address", podNode.Name)
		return false
	}

	if err := podNode.QueryStorageInfo(ctx); err != nil {
		log.Printf("Failed to query storage info for %s: %v", podNode.Name, err)
		return false
	}

	isEmpty := podNode.StorageInfo.EdgeCount == 0 && podNode.StorageInfo.VertexCount == 0
	log.Printf("Pod %s storage: %d vertices, %d edges (empty: %v)", 
		podNode.Name, podNode.StorageInfo.VertexCount, podNode.StorageInfo.EdgeCount, isEmpty)
	
	return isEmpty
}
