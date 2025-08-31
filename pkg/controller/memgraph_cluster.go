package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// MemgraphCluster handles all Memgraph cluster-specific operations
type MemgraphCluster struct {
	clientset      kubernetes.Interface
	config         *Config
	memgraphClient *MemgraphClient
	stateManager   StateManagerInterface
}

// NewMemgraphCluster creates a new MemgraphCluster instance
func NewMemgraphCluster(clientset kubernetes.Interface, config *Config, memgraphClient *MemgraphClient, stateManager StateManagerInterface) *MemgraphCluster {
	return &MemgraphCluster{
		clientset:      clientset,
		config:         config,
		memgraphClient: memgraphClient,
		stateManager:   stateManager,
	}
}

// DiscoverPods discovers running pods with the configured app name
func (mc *MemgraphCluster) DiscoverPods(ctx context.Context) (*ClusterState, error) {
	pods, err := mc.clientset.CoreV1().Pods(mc.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + mc.config.AppName,
	})
	if err != nil {
		return nil, err
	}

	clusterState := NewClusterState()

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
		clusterState.Pods[pod.Name] = podInfo

		log.Printf("Discovered pod: %s, IP: %s, Timestamp: %s",
			podInfo.Name,
			pod.Status.PodIP,
			podInfo.Timestamp.Format(time.RFC3339))
	}

	// Main selection will be done AFTER querying actual Memgraph state
	// This ensures we use real replication roles for decision making
	log.Printf("Pod discovery complete. Main selection deferred until after Memgraph querying.")

	return clusterState, nil
}

// GetPodsByLabel discovers pods matching the given label selector
func (mc *MemgraphCluster) GetPodsByLabel(ctx context.Context, labelSelector string) (*ClusterState, error) {
	pods, err := mc.clientset.CoreV1().Pods(mc.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	clusterState := NewClusterState()

	for _, pod := range pods.Items {
		podInfo := NewPodInfo(&pod)
		clusterState.Pods[pod.Name] = podInfo
	}

	return clusterState, nil
}

// getTargetMainIndex gets the target main index from state manager
func (mc *MemgraphCluster) getTargetMainIndex() int {
	if mc.stateManager == nil {
		return -1 // Bootstrap phase / test scenario
	}
	ctx := context.Background()
	state, err := mc.stateManager.LoadState(ctx)
	if err != nil {
		return -1
	}
	return state.MasterIndex
}

// updateTargetMainIndex updates the target main index in ConfigMap
func (mc *MemgraphCluster) updateTargetMainIndex(ctx context.Context, newIndex int, reason string) error {
	if mc.stateManager == nil {
		return fmt.Errorf("state manager not initialized")
	}

	// Load current state or create new one
	state, err := mc.stateManager.LoadState(ctx)
	if err != nil {
		// Create new state if none exists
		state = &ControllerState{
			MasterIndex: newIndex,
		}
	} else {
		// Update existing state
		oldIndex := state.MasterIndex
		state.MasterIndex = newIndex
		log.Printf("✅ Updated target main index: %d -> %d (%s)", oldIndex, newIndex, reason)
	}

	// Save updated state
	return mc.stateManager.SaveState(ctx, state)
}

// RefreshClusterInfo refreshes cluster state information in operational phase
func (mc *MemgraphCluster) RefreshClusterInfo(ctx context.Context, updateSyncReplicaInfoFunc func(*ClusterState), detectMainFailoverFunc func(*ClusterState) bool, handleMainFailoverFunc func(context.Context, *ClusterState) error) (*ClusterState, error) {
	log.Println("Refreshing Memgraph cluster information...")

	clusterState, err := mc.DiscoverPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover pods: %w", err)
	}

	// Update connection pool with fresh pod IPs
	if mc.memgraphClient != nil {
		for _, podInfo := range clusterState.Pods {
			if podInfo.Pod != nil && podInfo.Pod.Status.PodIP != "" {
				mc.memgraphClient.connectionPool.UpdatePodIP(podInfo.Name, podInfo.Pod.Status.PodIP)
			}
		}
	}

	if len(clusterState.Pods) == 0 {
		log.Println("No pods found in cluster")
		return clusterState, nil
	}

	// Always operational phase since bootstrap is handled separately
	log.Printf("Controller in operational phase - maintaining OPERATIONAL_STATE authority")
	clusterState.IsBootstrapPhase = false
	clusterState.StateType = OPERATIONAL_STATE

	clusterState.LastStateChange = time.Now()

	log.Printf("Discovered %d pods, querying Memgraph state...", len(clusterState.Pods))

	// Track errors for comprehensive reporting
	var queryErrors []error
	successCount := 0

	// Query Memgraph role and replicas for each pod
	for podName, podInfo := range clusterState.Pods {
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
		updateSyncReplicaInfoFunc(clusterState)
	}

	// Operational phase - detect and handle main failover scenarios (via callbacks)
	log.Println("Checking for main failover scenarios...")
	if detectMainFailoverFunc != nil && detectMainFailoverFunc(clusterState) {
		log.Printf("Main failover detected - handling failover...")
		if handleMainFailoverFunc != nil {
			if err := handleMainFailoverFunc(ctx, clusterState); err != nil {
				log.Printf("⚠️  Failover handling failed: %v", err)
				// Don't crash - log warning and continue as per design
			}
		}
	}
	
	// Set operational phase properties
	clusterState.IsBootstrapPhase = false
	clusterState.StateType = OPERATIONAL_STATE

	// Validate controller state consistency
	if warnings := clusterState.ValidateControllerState(mc.config); len(warnings) > 0 {
		log.Printf("⚠️  Controller state validation warnings:")
		for _, warning := range warnings {
			log.Printf("  - %s", warning)
		}
	}

	// NOW select main based on actual Memgraph state (not pod labels)
	log.Println("Selecting main based on actual Memgraph replication state...")
	mc.selectMainAfterQuerying(ctx, clusterState)

	// Log summary of query results
	log.Printf("Cluster discovery complete: %d/%d pods successfully queried",
		successCount, len(clusterState.Pods))

	if len(queryErrors) > 0 {
		log.Printf("Encountered %d errors during cluster discovery:", len(queryErrors))
		for _, err := range queryErrors {
			log.Printf("  - %v", err)
		}
	}

	return clusterState, nil
}

// selectMainAfterQuerying selects main based on actual Memgraph replication state
func (mc *MemgraphCluster) selectMainAfterQuerying(ctx context.Context, clusterState *ClusterState) {
	// After bootstrap validation, we now have authority to make decisions
	clusterState.IsBootstrapPhase = false

	// Enhanced main selection using controller state authority
	targetMainIndex := mc.getTargetMainIndex()
	log.Printf("Enhanced main selection: state=%s, target_index=%d",
		clusterState.StateType.String(), targetMainIndex)

	// Use controller state authority based on cluster state
	switch clusterState.StateType {
	case INITIAL_STATE:
		mc.applyDeterministicRoles(clusterState)
	case OPERATIONAL_STATE:
		mc.learnExistingTopology(clusterState)
	default:
		// For unknown states, use simple fallback as per DESIGN.md
		log.Printf("Unknown cluster state %s - using deterministic fallback", clusterState.StateType)
		mc.applyDeterministicRoles(clusterState)
	}

	// Log comprehensive cluster health summary
	healthSummary := clusterState.GetClusterHealthSummary()
	log.Printf("📋 CLUSTER HEALTH SUMMARY: %+v", healthSummary)
}

// applyDeterministicRoles applies roles for fresh/initial clusters
func (mc *MemgraphCluster) applyDeterministicRoles(clusterState *ClusterState) {
	log.Printf("Applying deterministic role assignment for fresh cluster")

	// Use the determined target main index
	targetMainIndex := mc.getTargetMainIndex()
	targetMainName := mc.config.GetPodName(targetMainIndex)
	clusterState.CurrentMain = targetMainName

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
	for podName := range clusterState.Pods {
		if podName != targetMainName && podName != syncReplicaName {
			asyncCount++
		}
	}
	log.Printf("  ASYNC replicas: %d pods", asyncCount)
}

// learnExistingTopology learns the current operational topology
func (mc *MemgraphCluster) learnExistingTopology(clusterState *ClusterState) {
	log.Printf("Learning existing operational topology")

	mainPods := clusterState.GetMainPods()
	if len(mainPods) == 1 {
		currentMain := mainPods[0]
		clusterState.CurrentMain = currentMain
		log.Printf("Learned existing main: %s", currentMain)

		// Extract current main index for tracking using consolidated method
		currentMainIndex := mc.config.ExtractPodIndex(currentMain)
		if currentMainIndex >= 0 {
			if err := mc.updateTargetMainIndex(context.Background(), currentMainIndex,
				fmt.Sprintf("Updating from discovered main %s", currentMain)); err != nil {
				log.Printf("Warning: failed to update target main index: %v", err)
			} else {
				log.Printf("Updated target main index to match existing: %d", currentMainIndex)
			}
		}

		// Log current SYNC replica
		for podName, podInfo := range clusterState.Pods {
			if podInfo.IsSyncReplica {
				log.Printf("Current SYNC replica: %s", podName)
				break
			}
		}

	} else {
		log.Printf("WARNING: Expected exactly 1 main in operational state, found %d: %v",
			len(mainPods), mainPods)

		// Use the determined target main as fallback
		targetMainIndex := mc.getTargetMainIndex()
		targetMainName := mc.config.GetPodName(targetMainIndex)
		clusterState.CurrentMain = targetMainName
		log.Printf("Using determined target main as fallback: %s", targetMainName)
	}
}

