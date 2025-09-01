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
	MemgraphNodes        map[string]*MemgraphNode
	CurrentMain string

	// Connection management
	connectionPool *ConnectionPool

	// Controller state tracking
	StateType        ClusterStateType
	IsBootstrapPhase bool // True during initial discovery
	BootstrapSafe    bool // True if bootstrap can proceed safely
	LastStateChange  time.Time

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
		MemgraphNodes:        make(map[string]*MemgraphNode),
		CurrentMain: "",

		// Connection management
		connectionPool: connectionPool,

		// State tracking
		StateType:        UNKNOWN_STATE,
		IsBootstrapPhase: false,
		BootstrapSafe:    false,
		LastStateChange:  time.Now(),

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

// RefreshClusterInfo refreshes cluster state information in operational phase
func (mc *MemgraphCluster) RefreshClusterInfo(ctx context.Context, updateSyncReplicaInfoFunc func(*MemgraphCluster), detectMainFailoverFunc func(*MemgraphCluster) bool, handleMainFailoverFunc func(context.Context, *MemgraphCluster) error) error {
	log.Println("Refreshing Memgraph cluster information...")

	err := mc.DiscoverPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover pods: %w", err)
	}

	// Connection pool is already shared between MemgraphClient and ClusterState

	if len(mc.MemgraphNodes) == 0 {
		log.Println("No pods found in cluster")
		return nil
	}

	// Always operational phase since bootstrap is handled separately
	log.Printf("Controller in operational phase - maintaining OPERATIONAL_STATE authority")
	mc.IsBootstrapPhase = false
	mc.StateType = OPERATIONAL_STATE

	mc.LastStateChange = time.Now()

	log.Printf("Discovered %d pods, querying Memgraph state...", len(mc.MemgraphNodes))

	// Track errors for comprehensive reporting
	var queryErrors []error
	successCount := 0

	// Query Memgraph role and replicas for each pod
	for podName, node := range mc.MemgraphNodes {
		if node.BoltAddress == "" {
			log.Printf("Skipping pod %s: no Bolt address", podName)
			continue
		}

		log.Printf("Querying replication role for pod %s at %s", podName, node.BoltAddress)

		// Query replication role with retry
		role, err := node.QueryReplicationRole(ctx)
		if err != nil {
			log.Printf("Failed to query replication role for pod %s: %v", podName, err)
			queryErrors = append(queryErrors, fmt.Errorf("pod %s role query: %w", podName, err))
			continue
		}

		node.MemgraphRole = role.Role
		log.Printf("Pod %s has Memgraph role: %s", podName, role.Role)

		// If this is a MAIN node, query its replicas
		if role.Role == "main" {
			log.Printf("Querying replicas for main pod %s", podName)

			replicasResp, err := node.QueryReplicas(ctx)
			if err != nil {
				log.Printf("Failed to query replicas for pod %s: %v", podName, err)
				queryErrors = append(queryErrors, fmt.Errorf("pod %s replicas query: %w", podName, err))
			} else {
				// Store detailed replica information including sync mode
				node.ReplicasInfo = replicasResp.Replicas

				// Extract replica names for backward compatibility
				var replicaNames []string
				for _, replica := range replicasResp.Replicas {
					replicaNames = append(replicaNames, replica.Name)
				}
				node.Replicas = replicaNames

				// Log detailed replica information including sync modes
				log.Printf("Pod %s has %d replicas:", podName, len(replicaNames))
				for _, replica := range replicasResp.Replicas {
					log.Printf("  - %s (sync_mode: %s)", replica.Name, replica.SyncMode)
				}
			}
		}

		// Classify the pod state based on collected information
		newState := node.ClassifyState()
		if newState != node.State {
			log.Printf("Pod %s state changed from %s to %s", podName, node.State, newState)
			node.State = newState
		}

		// Check for state inconsistencies
		if inconsistency := node.DetectStateInconsistency(); inconsistency != nil {
			log.Printf("WARNING: State inconsistency detected for pod %s: %s",
				podName, inconsistency.Description)
		}

		successCount++
	}

	// Update SYNC replica information based on actual main's replica data (via callback)
	if updateSyncReplicaInfoFunc != nil {
		updateSyncReplicaInfoFunc(mc)
	}

	// Operational phase - detect and handle main failover scenarios (via callbacks)
	log.Println("Checking for main failover scenarios...")
	if detectMainFailoverFunc != nil && detectMainFailoverFunc(mc) {
		log.Printf("Main failover detected - handling failover...")
		if handleMainFailoverFunc != nil {
			if err := handleMainFailoverFunc(ctx, mc); err != nil {
				log.Printf("⚠️  Failover handling failed: %v", err)
				// Don't crash - log warning and continue as per design
			}
		}
	}

	// Set operational phase properties
	mc.IsBootstrapPhase = false
	mc.StateType = OPERATIONAL_STATE

	// Validate controller state consistency
	if warnings := mc.ValidateControllerState(mc.config); len(warnings) > 0 {
		log.Printf("⚠️  Controller state validation warnings:")
		for _, warning := range warnings {
			log.Printf("  - %s", warning)
		}
	}

	// Controller will call selectMainAfterQuerying separately

	// Log summary of query results
	log.Printf("Cluster discovery complete: %d/%d pods successfully queried",
		successCount, len(mc.MemgraphNodes))

	if len(queryErrors) > 0 {
		log.Printf("Encountered %d errors during cluster discovery:", len(queryErrors))
		for _, err := range queryErrors {
			log.Printf("  - %v", err)
		}
	}

	return nil
}

// selectMainAfterQuerying selects main based on actual Memgraph replication state
func (mc *MemgraphCluster) selectMainAfterQuerying(ctx context.Context, targetMainIndex int) {
	// After bootstrap validation, we now have authority to make decisions
	mc.IsBootstrapPhase = false

	// Enhanced main selection using controller state authority
	log.Printf("Enhanced main selection: state=%s, target_index=%d",
		mc.StateType.String(), targetMainIndex)

	// Use controller state authority based on cluster state
	switch mc.StateType {
	case INITIAL_STATE:
		mc.applyDeterministicRoles(targetMainIndex)
	case OPERATIONAL_STATE:
		mc.learnExistingTopology(targetMainIndex)
	default:
		// For unknown states, use simple fallback as per DESIGN.md
		log.Printf("Unknown cluster state %s - using deterministic fallback", mc.StateType)
		mc.applyDeterministicRoles(targetMainIndex)
	}

	// Log comprehensive cluster health summary
	healthSummary := mc.GetClusterHealthSummary(targetMainIndex)
	log.Printf("📋 CLUSTER HEALTH SUMMARY: %+v", healthSummary)
}

// applyDeterministicRoles applies roles for fresh/initial clusters
func (mc *MemgraphCluster) applyDeterministicRoles(targetMainIndex int) {
	log.Printf("Applying deterministic role assignment for fresh cluster")

	// Use the determined target main index
	targetMainName := mc.config.GetPodName(targetMainIndex)
	mc.CurrentMain = targetMainName

	log.Printf("Deterministic main assignment: %s (index %d)",
		targetMainName, targetMainIndex)

	// Log planned topology
	syncReplicaIndex := 1 - targetMainIndex // 0->1, 1->0
	syncReplicaName := mc.config.GetPodName(syncReplicaIndex)

	log.Printf("Planned topology:")
	log.Printf("  Main: %s (index %d)", targetMainName, targetMainIndex)
	log.Printf("  SYNC replica: %s (index %d)", syncReplicaName, syncReplicaIndex)

	// Mark remaining pods as ASYNC replicas
	asyncCount := 0
	for podName := range mc.MemgraphNodes {
		if podName != targetMainName && podName != syncReplicaName {
			asyncCount++
		}
	}
	log.Printf("  ASYNC replicas: %d pods", asyncCount)
}

// learnExistingTopology learns the current operational topology
func (mc *MemgraphCluster) learnExistingTopology(targetMainIndex int) {
	log.Printf("Learning existing operational topology")

	mainPods := mc.GetMainPods()
	if len(mainPods) == 1 {
		currentMain := mainPods[0]
		mc.CurrentMain = currentMain
		log.Printf("Learned existing main: %s", currentMain)

		// The controller should update the target main index based on discovered main
		currentMainIndex := mc.config.ExtractPodIndex(currentMain)
		log.Printf("Discovered existing main index: %d", currentMainIndex)

		// Log current SYNC replica
		for podName, node := range mc.MemgraphNodes {
			if node.IsSyncReplica {
				log.Printf("Current SYNC replica: %s", podName)
				break
			}
		}

	} else {
		log.Printf("WARNING: Expected exactly 1 main in operational state, found %d: %v",
			len(mainPods), mainPods)

		// Use the determined target main as fallback
		targetMainName := mc.config.GetPodName(targetMainIndex)
		mc.CurrentMain = targetMainName
		log.Printf("Using determined target main as fallback: %s", targetMainName)
	}
}

// GetClusterHealthSummary returns a comprehensive health summary of the cluster
func (mc *MemgraphCluster) GetClusterHealthSummary(targetIndex int) map[string]interface{} {
	healthyPods := 0
	totalPods := len(mc.MemgraphNodes)
	syncReplicaCount := 0
	mainPods := 0
	replicaPods := 0

	for _, node := range mc.MemgraphNodes {
		if node.BoltAddress != "" && node.MemgraphRole != "" {
			healthyPods++
		}

		if node.IsSyncReplica {
			syncReplicaCount++
		}

		switch node.MemgraphRole {
		case "main":
			mainPods++
		case "replica":
			replicaPods++
		}
	}

	// Target index provided as parameter

	return map[string]interface{}{
		"total_pods":      totalPods,
		"healthy_pods":    healthyPods,
		"unhealthy_pods":  totalPods - healthyPods,
		"main_pods":       mainPods,
		"replica_pods":    replicaPods,
		"sync_replicas":   syncReplicaCount,
		"state_type":      mc.StateType.String(),
		"bootstrap_phase": mc.IsBootstrapPhase,
		"current_main":    mc.CurrentMain,
		"target_index":    targetIndex,
		"last_change":     mc.LastStateChange,
	}
}

// ValidateControllerState validates the controller state consistency
func (mc *MemgraphCluster) ValidateControllerState(config *Config) []string {
	var warnings []string

	// Validate current main exists in pods
	if mc.CurrentMain != "" {
		if _, exists := mc.MemgraphNodes[mc.CurrentMain]; !exists {
			warnings = append(warnings, fmt.Sprintf("Current main '%s' not found in discovered pods", mc.CurrentMain))
		}
	}

	return warnings
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
	log.Printf("  State Type: %s", metrics.StateType.String())
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
func (mc *MemgraphCluster) discoverClusterState(ctx context.Context) error {
	log.Println("=== DISCOVERING CLUSTER STATE ===")

	// Step 1: If kubernetes status of either pod-0 or pod-1 is not ready, log warning and stop
	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Info, pod0Exists := mc.MemgraphNodes[pod0Name]
	pod1Info, pod1Exists := mc.MemgraphNodes[pod1Name]

	if !pod0Exists || !pod1Exists || !isPodReady(pod0Info.Pod) || !isPodReady(pod1Info.Pod) {
		return fmt.Errorf("DESIGN.md step 1: pod-0 or pod-1 not ready - cannot proceed with discovery")
	}

	// Query Memgraph roles for both pods
	if err := mc.queryMemgraphRoles(ctx); err != nil {
		return fmt.Errorf("failed to query Memgraph roles: %w", err)
	}

	// Step 2: If both pod-0 and pod-1 have replication role as `MAIN` and storage shows 0 edge_count, 0 vertex_count
	if mc.isBothMainWithEmptyStorage(ctx) {
		log.Println("✅ DESIGN.md step 2: INITIAL_STATE detected (both MAIN, empty storage)")
		mc.StateType = INITIAL_STATE
		return nil
	}

	// Step 3: If one of pod-0 and pod-1 has replication role as `REPLICA`, the other one as `MAIN`
	if mainPodName := mc.getSingleMainPod(); mainPodName != "" {
		log.Printf("✅ DESIGN.md step 3: OPERATIONAL_STATE detected (main: %s)", mainPodName)
		mc.StateType = OPERATIONAL_STATE
		mc.CurrentMain = mainPodName
		return nil
	}

	// Step 4: Otherwise, memgraph-ha is in an unknown state, controller log error and crash immediately
	log.Printf("❌ DESIGN.md step 4: UNKNOWN_STATE detected")
	log.Printf("Pod roles: pod-0=%s, pod-1=%s", pod0Info.MemgraphRole, pod1Info.MemgraphRole)
	mc.StateType = UNKNOWN_STATE
	return fmt.Errorf("UNKNOWN_STATE: controller must crash - manual intervention required")
}

// initializeCluster implements DESIGN.md "Initialize Memgraph Cluster" section
func (mc *MemgraphCluster) initializeCluster(ctx context.Context) error {
	log.Println("=== INITIALIZING MEMGRAPH CLUSTER ===")
	log.Println("Controller always use pod-0 as MAIN, pod-1 as SYNC REPLICA")

	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Info := mc.MemgraphNodes[pod0Name]
	pod1Info := mc.MemgraphNodes[pod1Name]

	if pod0Info == nil || pod1Info == nil {
		return fmt.Errorf("pod-0 or pod-1 not found for initialization")
	}

	// Step 1: Run command against pod-1 to demote it into replica
	log.Printf("Step 1: Demoting pod-1 (%s) to replica role", pod1Name)
	if err := pod1Info.SetToReplicaRole(ctx); err != nil {
		return fmt.Errorf("step 1 failed - demote pod-1 to replica: %w", err)
	}

	// Step 2: Run command against pod-0 to set up sync replication
	log.Printf("Step 2: Setting up SYNC replication from pod-0 to pod-1")
	replicaName := pod1Info.GetReplicaName()
	replicaAddress := pod1Info.GetReplicationAddress()

	if err := pod0Info.RegisterReplica(ctx, replicaName, replicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("step 2 failed - register SYNC replica: %w", err)
	}

	// Step 3: Run command against pod-0 to verify replication
	log.Printf("Step 3: Verifying replication status")
	if err := mc.verifyReplicationWithRetry(ctx, pod0Info, replicaName); err != nil {
		return fmt.Errorf("step 3 failed - replication verification: %w", err)
	}

	// Update cluster state to reflect initialization
	mc.CurrentMain = pod0Name
	pod1Info.IsSyncReplica = true
	mc.StateType = OPERATIONAL_STATE

	log.Printf("✅ Initialize Memgraph Cluster completed: pod-0 is MAIN, pod-1 is SYNC REPLICA")
	return nil
}

// queryMemgraphRoles queries replication roles from both pods
func (mc *MemgraphCluster) queryMemgraphRoles(ctx context.Context) error {
	for podName, node := range mc.MemgraphNodes {
		if !mc.config.IsMemgraphPod(podName) {
			continue
		}

		if node.BoltAddress == "" {
			log.Printf("Skipping role query for %s: no bolt address", podName)
			continue
		}

		roleResp, err := node.QueryReplicationRole(ctx)
		if err != nil {
			log.Printf("Failed to query role for %s: %v", podName, err)
			continue
		}

		node.MemgraphRole = roleResp.Role
		log.Printf("Pod %s has Memgraph role: %s", podName, roleResp.Role)
	}

	return nil
}

// isBothMainWithEmptyStorage checks DESIGN.md step 2 condition
func (mc *MemgraphCluster) isBothMainWithEmptyStorage(ctx context.Context) bool {
	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Info := mc.MemgraphNodes[pod0Name]
	pod1Info := mc.MemgraphNodes[pod1Name]

	// Both must have MAIN role
	if pod0Info.MemgraphRole != "main" || pod1Info.MemgraphRole != "main" {
		return false
	}

	// Both must have empty storage
	return mc.hasEmptyStorage(ctx, pod0Info) && mc.hasEmptyStorage(ctx, pod1Info)
}

// getSingleMainPod checks DESIGN.md step 3 condition - returns main pod name if exactly one main found
func (mc *MemgraphCluster) getSingleMainPod() string {
	pod0Name := mc.config.GetPodName(0)
	pod1Name := mc.config.GetPodName(1)

	pod0Info := mc.MemgraphNodes[pod0Name]
	pod1Info := mc.MemgraphNodes[pod1Name]

	if pod0Info == nil || pod1Info == nil {
		return ""
	}

	// Check if exactly one is main and one is replica
	if pod0Info.MemgraphRole == "main" && pod1Info.MemgraphRole == "replica" {
		return pod0Name
	}
	if pod1Info.MemgraphRole == "main" && pod0Info.MemgraphRole == "replica" {
		return pod1Name
	}

	return ""
}

// hasEmptyStorage checks if pod has empty storage (0 edges, 0 vertices)
func (mc *MemgraphCluster) hasEmptyStorage(ctx context.Context, node *MemgraphNode) bool {
	if node.BoltAddress == "" {
		log.Printf("Cannot check storage for pod %s: no bolt address", node.Name)
		return false
	}

	storageInfo, err := node.QueryStorageInfo(ctx)
	if err != nil {
		log.Printf("Failed to query storage info for %s: %v", node.Name, err)
		return false
	}

	isEmpty := storageInfo.EdgeCount == 0 && storageInfo.VertexCount == 0
	log.Printf("Pod %s storage: %d vertices, %d edges (empty: %v)",
		node.Name, storageInfo.VertexCount, storageInfo.EdgeCount, isEmpty)

	return isEmpty
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
