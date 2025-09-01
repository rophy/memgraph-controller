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
	Pods        map[string]*PodInfo
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
		Pods:        make(map[string]*PodInfo),
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
	mc.Pods = make(map[string]*PodInfo)

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

		podInfo := NewPodInfo(&pod)
		mc.Pods[pod.Name] = podInfo

		log.Printf("Discovered pod: %s, IP: %s, Timestamp: %s",
			podInfo.Name,
			pod.Status.PodIP,
			podInfo.Timestamp.Format(time.RFC3339))
	}

	// Main selection will be done AFTER querying actual Memgraph state
	// This ensures we use real replication roles for decision making
	log.Printf("Pod discovery complete. Main selection deferred until after Memgraph querying.")

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
	mc.Pods = make(map[string]*PodInfo)

	for _, pod := range pods.Items {
		podInfo := NewPodInfo(&pod)
		mc.Pods[pod.Name] = podInfo
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

	if len(mc.Pods) == 0 {
		log.Println("No pods found in cluster")
		return nil
	}

	// Always operational phase since bootstrap is handled separately
	log.Printf("Controller in operational phase - maintaining OPERATIONAL_STATE authority")
	mc.IsBootstrapPhase = false
	mc.StateType = OPERATIONAL_STATE

	mc.LastStateChange = time.Now()

	log.Printf("Discovered %d pods, querying Memgraph state...", len(mc.Pods))

	// Track errors for comprehensive reporting
	var queryErrors []error
	successCount := 0

	// Query Memgraph role and replicas for each pod
	for podName, podInfo := range mc.Pods {
		if podInfo.BoltAddress == "" {
			log.Printf("Skipping pod %s: no Bolt address", podName)
			continue
		}

		log.Printf("Querying replication role for pod %s at %s", podName, podInfo.BoltAddress)

		// Query replication role with retry
		role, err := mc.memgraphClient.QueryReplicationRoleWithRetry(ctx, podInfo.BoltAddress)
		if err != nil {
			log.Printf("Failed to query replication role for pod %s: %v", podName, err)
			queryErrors = append(queryErrors, fmt.Errorf("pod %s role query: %w", podName, err))
			continue
		}

		podInfo.MemgraphRole = role.Role
		log.Printf("Pod %s has Memgraph role: %s", podName, role.Role)

		// If this is a MAIN node, query its replicas
		if role.Role == "main" {
			log.Printf("Querying replicas for main pod %s", podName)

			replicasResp, err := mc.memgraphClient.QueryReplicasWithRetry(ctx, podInfo.BoltAddress)
			if err != nil {
				log.Printf("Failed to query replicas for pod %s: %v", podName, err)
				queryErrors = append(queryErrors, fmt.Errorf("pod %s replicas query: %w", podName, err))
			} else {
				// Store detailed replica information including sync mode
				podInfo.ReplicasInfo = replicasResp.Replicas

				// Extract replica names for backward compatibility
				var replicaNames []string
				for _, replica := range replicasResp.Replicas {
					replicaNames = append(replicaNames, replica.Name)
				}
				podInfo.Replicas = replicaNames

				// Log detailed replica information including sync modes
				log.Printf("Pod %s has %d replicas:", podName, len(replicaNames))
				for _, replica := range replicasResp.Replicas {
					log.Printf("  - %s (sync_mode: %s)", replica.Name, replica.SyncMode)
				}
			}
		}

		// Classify the pod state based on collected information
		newState := podInfo.ClassifyState()
		if newState != podInfo.State {
			log.Printf("Pod %s state changed from %s to %s", podName, podInfo.State, newState)
			podInfo.State = newState
		}

		// Check for state inconsistencies
		if inconsistency := podInfo.DetectStateInconsistency(); inconsistency != nil {
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
		successCount, len(mc.Pods))

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
	for podName := range mc.Pods {
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
		for podName, podInfo := range mc.Pods {
			if podInfo.IsSyncReplica {
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
	totalPods := len(mc.Pods)
	syncReplicaCount := 0
	mainPods := 0
	replicaPods := 0

	for _, podInfo := range mc.Pods {
		if podInfo.BoltAddress != "" && podInfo.MemgraphRole != "" {
			healthyPods++
		}

		if podInfo.IsSyncReplica {
			syncReplicaCount++
		}

		switch podInfo.MemgraphRole {
		case "main":
			mainPods++
		case "replica":
			replicaPods++
		}
	}

	// Target index provided as parameter

	return map[string]interface{}{
		"total_pods":       totalPods,
		"healthy_pods":     healthyPods,
		"unhealthy_pods":   totalPods - healthyPods,
		"main_pods":        mainPods,
		"replica_pods":     replicaPods,
		"sync_replicas":    syncReplicaCount,
		"state_type":       mc.StateType.String(),
		"bootstrap_phase":  mc.IsBootstrapPhase,
		"current_main":     mc.CurrentMain,
		"target_index":     targetIndex,
		"last_change":      mc.LastStateChange,
	}
}

// ValidateControllerState validates the controller state consistency
func (mc *MemgraphCluster) ValidateControllerState(config *Config) []string {
	var warnings []string

	// Validate current main exists in pods
	if mc.CurrentMain != "" {
		if _, exists := mc.Pods[mc.CurrentMain]; !exists {
			warnings = append(warnings, fmt.Sprintf("Current main '%s' not found in discovered pods", mc.CurrentMain))
		}
	}

	return warnings
}

// GetMainPods returns a list of pod names that have the MAIN role
func (mc *MemgraphCluster) GetMainPods() []string {
	var mainPods []string
	for podName, podInfo := range mc.Pods {
		if podInfo.MemgraphRole == "main" {
			mainPods = append(mainPods, podName)
		}
	}
	return mainPods
}

// GetReplicaPods returns a list of pod names that have the REPLICA role
func (mc *MemgraphCluster) GetReplicaPods() []string {
	var replicaPods []string
	for podName, podInfo := range mc.Pods {
		if podInfo.MemgraphRole == "replica" {
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
	
	if podInfo, exists := mc.Pods[podName]; exists && podInfo.BoltAddress != "" {
		mc.connectionPool.InvalidateConnection(podInfo.BoltAddress)
		log.Printf("Invalidated connection for pod %s (%s)", podName, podInfo.BoltAddress)
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
	if podInfo, exists := mc.Pods[podName]; exists {
		newBoltAddress := ""
		if newIP != "" {
			newBoltAddress = newIP + ":7687"
		}
		podInfo.BoltAddress = newBoltAddress
	}
}

// CloseAllConnections closes all connections in the connection pool
func (mc *MemgraphCluster) CloseAllConnections(ctx context.Context) {
	if mc.connectionPool != nil {
		mc.connectionPool.Close(ctx)
	}
}

