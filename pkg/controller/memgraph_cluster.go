package controller

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	"memgraph-controller/pkg/common"
)

// MemgraphCluster handles all Memgraph cluster-specific operations and represents the cluster state
type MemgraphCluster struct {
	// Cluster data (formerly ClusterState)
	MemgraphNodes map[string]*MemgraphNode

	// External dependencies
	podCacheStore  cache.Store
	config         *common.Config
	memgraphClient *MemgraphClient
}

// NewMemgraphCluster creates a new MemgraphCluster instance
func NewMemgraphCluster(podCacheStore cache.Store, config *common.Config, memgraphClient *MemgraphClient) *MemgraphCluster {

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
func (mc *MemgraphCluster) Refresh(ctx context.Context) error {
	podList := mc.getPodsFromCache()

	// Create a map of podName to pod for quick lookup
	podMap := make(map[string]v1.Pod)
	for _, pod := range podList {
		podMap[pod.Name] = pod
	}

	// Iterate through current pods in the cluster and disconnect any that no longer exist
	for podName := range mc.MemgraphNodes {
		if _, exists := podMap[podName]; !exists {
			logger.Info("pod no longer exists - disconnecting", "pod_name", podName)
			if err := mc.MemgraphNodes[podName].InvalidateConnection(); err != nil {
				logger.Warn("failed to invalidate connection for pod", "pod_name", podName, "error", err)
			}
		}
	}

	// Iterate through discovered pods and update or add them to the cluster state
	for podName := range podMap {
		pod := podMap[podName]
		_, exists := mc.MemgraphNodes[podName]
		if !exists {
			// New pod - create MemgraphNode
			logger.Info("discovered new pod", "pod_name", podName)
			node := NewMemgraphNode(&pod, mc.memgraphClient)
			mc.MemgraphNodes[podName] = node
		} else {
			mc.MemgraphNodes[podName].Refresh(&pod)
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

// getPodsFromCache retrieves all memgraph pods from the informer cache
func (mc *MemgraphCluster) getPodsFromCache() []v1.Pod {
	objects := mc.podCacheStore.List()
	pods := make([]v1.Pod, 0, len(objects))

	for _, obj := range objects {
		if pod, ok := obj.(*v1.Pod); ok {
			pods = append(pods, *pod)
		}
	}

	return pods
}

// discoverClusterState implements DESIGN.md "Discover Cluster State" section (steps 1-4)
// Returns (targetMainIndex, nil) if cluster is ready
// Returns (-1, nil) if cluster is not ready to be discovered
// Returns (-1, error) if cluster is in an unknown state
func (mc *MemgraphCluster) discoverClusterState(ctx context.Context) (int, error) {
	start := time.Now()
	defer func() {
		logger.Info("discoverClusterState completed", "duration_ms", float64(time.Since(start).Nanoseconds())/1e6)
	}()

	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	// discoverClusterState step 1
	logger.Info("discoverClusterState step 1", "pod0_name", pod0Name, "pod1_name", pod1Name)

	err := mc.Refresh(ctx)
	if err != nil {
		logger.Warn("failed to refresh cluster state", "error", err)
		return -1, nil
	}
	// The key for podCacheStore is "namespace/podName"
	pod0Key := fmt.Sprintf("%s/%s", mc.config.Namespace, pod0Name)
	pod0Obj, pod0Exists, pod0Err := mc.podCacheStore.GetByKey(pod0Key)
	if pod0Err != nil {
		logger.Warn("failed to get pod0 from cache", "pod0_key", pod0Key, "error", pod0Err)
		return -1, nil
	}
	if !pod0Exists {
		logger.Warn("pod0 does not exist", "pod0_key", pod0Key)
		return -1, nil
	}
	if !isPodReady(pod0Obj.(*v1.Pod)) {
		logger.Warn("pod0 is not ready", "pod0_key", pod0Key)
		return -1, nil
	}

	pod1Key := fmt.Sprintf("%s/%s", mc.config.Namespace, pod1Name)
	pod1Obj, pod1Exists, pod1Err := mc.podCacheStore.GetByKey(pod1Key)
	if pod1Err != nil {
		logger.Warn("failed to get pod1 from cache", "pod1_key", pod1Key, "error", pod1Err)
		return -1, nil
	}
	if !pod1Exists {
		logger.Warn("pod1 does not exist", "pod1_key", pod1Key)
		return -1, nil
	}
	if !isPodReady(pod1Obj.(*v1.Pod)) {
		logger.Warn("pod1 is not ready", "pod1_key", pod1Key)
		return -1, nil
	}

	// discoverClusterState step 2
	logger.Info("discoverClusterState step 2", "pod0_name", pod0Name, "pod1_name", pod1Name)
	isNew, err := mc.isNewCluster(ctx)
	if err != nil {
		return -1, fmt.Errorf("failed to check if this is a new cluster: %w", err)
	}
	if isNew {
		logger.Info("discoverClusterState step 2: discovered new cluster")
		if err := mc.initializeCluster(ctx); err != nil {
			return -1, fmt.Errorf("failed to initialize cluster: %w", err)
		}
		return 0, nil // pod-0 becomes main
	}

	// discoverClusterState step 3
	logger.Info("discoverClusterState step 3", "pod0_name", pod0Name, "pod1_name", pod1Name)
	pod0Role, err := mc.MemgraphNodes[pod0Name].GetReplicationRole(ctx)
	if err != nil {
		logger.Error("failed to get replication role for pod0", "pod0_name", pod0Name, "error", err)
		return -1, fmt.Errorf("failed to get replication role for %s: %w", pod0Name, err)
	}
	pod1Role, err := mc.MemgraphNodes[pod1Name].GetReplicationRole(ctx)
	if err != nil {
		logger.Error("failed to get replication role for pod1", "pod1_name", pod1Name, "error", err)
		return -1, fmt.Errorf("failed to get replication role for %s: %w", pod1Name, err)
	}
	logger.Info("discoverClusterState step 3: pod roles", "pod0_name", pod0Name, "pod0_role", pod0Role, "pod1_name", pod1Name, "pod1_role", pod1Role)
	if pod0Role == "main" && pod1Role == "replica" {
		logger.Info("discoverClusterState step 3: pod0 is main, pod1 is sync replica", "pod0_name", pod0Name, "pod1_name", pod1Name)
		return 0, nil
	}
	if pod0Role == "replica" && pod1Role == "main" {
		logger.Info("discoverClusterState step 3: pod0 is sync replica, pod1 is main", "pod0_name", pod0Name, "pod1_name", pod1Name)
		return 1, nil
	}

	// discoverClusterState step 4: memgraph-ha is in an unknown state
	logger.Error("discoverClusterState step 4: cluster is in an unknown state - manual intervention required")
	return -1, fmt.Errorf("cluster is in an unknown state - manual intervention required")
}

// isNewCluster checks if the cluster is new
func (mc *MemgraphCluster) isNewCluster(ctx context.Context) (bool, error) {

	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)
	node0 := mc.MemgraphNodes[pod0Name]
	node1 := mc.MemgraphNodes[pod1Name]

	pod0Role, err := node0.GetReplicationRole(ctx)
	if err != nil {
		logger.Error("failed to get replication role for pod0", "pod0_name", pod0Name, "error", err)
		return false, fmt.Errorf("failed to get replication role for %s: %w", pod0Name, err)
	}
	pod1Role, err := node1.GetReplicationRole(ctx)
	if err != nil {
		logger.Error("failed to get replication role for pod1", "pod1_name", pod1Name, "error", err)
		return false, fmt.Errorf("failed to get replication role for %s: %w", pod1Name, err)
	}

	if pod0Role != "main" || pod1Role != "main" {
		return false, nil
	}
	if err := node0.QueryStorageInfo(ctx); err != nil {
		return false, fmt.Errorf("failed to query storage info for %s: %w", pod0Name, err)
	}
	if node0.storageInfo.EdgeCount != 0 || node0.storageInfo.VertexCount != 0 {
		return false, nil
	}
	if err := node1.QueryStorageInfo(ctx); err != nil {
		return false, fmt.Errorf("failed to query storage info for %s: %w", pod1Name, err)
	}
	if node1.storageInfo.EdgeCount != 0 || node1.storageInfo.VertexCount != 0 {
		return false, nil
	}

	// Both pods are main and empty, this is a new cluster
	return true, nil
}

// initializeCluster implements DESIGN.md "Initialize Memgraph Cluster" section
func (mc *MemgraphCluster) initializeCluster(ctx context.Context) error {
	start := time.Now()
	defer func() {
		logger.Info("initializeCluster completed", "duration_ms", float64(time.Since(start).Nanoseconds())/1e6)
	}()
	
	logger.Info("initializing memgraph cluster")

	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Node := mc.MemgraphNodes[pod0Name]
	pod1Node := mc.MemgraphNodes[pod1Name]

	if pod0Node == nil || pod1Node == nil {
		return fmt.Errorf("pod-0 or pod-1 not found for initialization")
	}

	// Step 1: Run command against pod-1 to demote it into replica
	logger.Info("step 1: demoting pod-1 to replica role", "pod1_name", pod1Name)
	if err := pod1Node.SetToReplicaRole(ctx); err != nil {
		return fmt.Errorf("step 1 failed - demote pod-1 to replica: %w", err)
	}

	// Step 2: Run command against pod-0 to set up sync replication
	logger.Info("step 2: setting up sync replication from pod-0 to pod-1", "pod0_name", pod0Name, "pod1_name", pod1Name)
	if err := pod0Node.RegisterReplica(ctx, pod1Node.GetReplicaName(), pod1Node.ipAddress, "SYNC"); err != nil {
		return fmt.Errorf("step 2 failed - register SYNC replica: %w", err)
	}

	// Step 3: Run command against pod-0 to verify replication
	logger.Info("step 3: verifying replication status")
	replicasResponse, err := pod0Node.GetReplicas(ctx)
	if err != nil {
		return fmt.Errorf("step 3 failed - query replicas: %w", err)
	}

	// Check if replica shows as ready
	found := false
	for _, replica := range replicasResponse {
		logger.Info("replica found", "replica_name", replica.Name, "sync_mode", replica.SyncMode)
		if replica.Name == pod1Node.GetReplicaName() && replica.SyncMode == "sync" {
			found = true
			// Parse data_info to check if replica is ready
			if replica.ParsedDataInfo != nil && replica.ParsedDataInfo.Status == "ready" && replica.ParsedDataInfo.Behind == 0 {
				logger.Info("sync replica is ready and up-to-date", "replica_name", replica.Name)
			} else {
				return fmt.Errorf("SYNC replica %s is not ready: data_info=%s", replica.Name, replica.DataInfo)
			}
			break
		}
	}
	if !found {
		return fmt.Errorf("SYNC replica %s not found in SHOW REPLICAS output", pod1Node.GetReplicaName())
	}

	logger.Info("initialize memgraph cluster completed", "main", pod0Name, "sync_replica", pod1Name)
	return nil
}
