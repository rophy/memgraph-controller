package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// MemgraphCluster handles all Memgraph cluster-specific operations and represents the cluster state
type MemgraphCluster struct {
	// Cluster data (formerly ClusterState)
	MemgraphNodes map[string]*MemgraphNode
	CurrentMain   string

	// External dependencies
	clientset      kubernetes.Interface
	config         *Config
	memgraphClient *MemgraphClient
}

// NewMemgraphCluster creates a new MemgraphCluster instance
func NewMemgraphCluster(clientset kubernetes.Interface, config *Config, memgraphClient *MemgraphClient) *MemgraphCluster {

	cluster := &MemgraphCluster{
		// Initialize cluster data
		MemgraphNodes: make(map[string]*MemgraphNode),
		CurrentMain:   "",

		// External dependencies
		clientset:      clientset,
		config:         config,
		memgraphClient: memgraphClient,
	}

	return cluster
}

// DiscoverPods discovers running pods with the configured app name and updates cluster state
func (mc *MemgraphCluster) DiscoverPods(ctx context.Context) error {
	podList, err := mc.clientset.CoreV1().Pods(mc.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + mc.config.AppName,
	})
	if err != nil {
		return err
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
			mc.MemgraphNodes[podName].PodExists = false
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
		err := node.QueryReplicationRole(ctx)
		if err != nil {
			log.Printf("Failed to query replication role for pod %s: %v", podName, err)
			queryErrors = append(queryErrors, fmt.Errorf("pod %s role query: %w", podName, err))
			continue
		}

		// MemgraphRole is set by QueryReplicationRole internally
		log.Printf("Pod %s has Memgraph role: %s", podName, node.MemgraphRole)

		// If this is a MAIN node, query its replicas
		if node.MemgraphRole == "main" {
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

		// Classify the pod state based on collected information (TODO: implement state classification)
		// newState := node.ClassifyState()
		// if newState != node.State {
		// 	log.Printf("Pod %s state changed from %s to %s", podName, node.State, newState)
		// 	node.State = newState
		// }

		// Check for state inconsistencies (TODO: implement inconsistency detection)
		// if inconsistency := node.DetectStateInconsistency(); inconsistency != nil {
		//	log.Printf("WARNING: State inconsistency detected for pod %s: %s",
		//		podName, inconsistency.Description)
		// }

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

	// Operational phase properties set (no tracking needed)

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
	log.Printf("  Selected Main: %s", metrics.SelectedMain)
	log.Printf("  Selection Reason: %s", metrics.SelectionReason)
	log.Printf("  Healthy Pods: %d", metrics.HealthyPodsCount)
	log.Printf("  SYNC Replica Available: %t", metrics.SyncReplicaAvailable)
	log.Printf("  Failover Detected: %t", metrics.FailoverDetected)
	log.Printf("  Decision Factors: %v", metrics.DecisionFactors)
}

// Unused methods discoverClusterState, initializeCluster, verifyReplicationWithRetry, isReplicaReady removed
