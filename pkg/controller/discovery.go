package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type PodDiscovery struct {
	clientset kubernetes.Interface
	config    *Config
}

func NewPodDiscovery(clientset kubernetes.Interface, config *Config) *PodDiscovery {
	return &PodDiscovery{
		clientset: clientset,
		config:    config,
	}
}

func (pd *PodDiscovery) DiscoverPods(ctx context.Context) (*ClusterState, error) {
	pods, err := pd.clientset.CoreV1().Pods(pd.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + pd.config.AppName,
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

func (pd *PodDiscovery) GetPodsByLabel(ctx context.Context, labelSelector string) (*ClusterState, error) {
	pods, err := pd.clientset.CoreV1().Pods(pd.config.Namespace).List(ctx, metav1.ListOptions{
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

// DiscoverCluster discovers the current state of the Memgraph cluster
func (c *MemgraphController) DiscoverCluster(ctx context.Context) (*ClusterState, error) {
	if c.isBootstrap {
		log.Println("=== BOOTSTRAP PHASE ===")
		log.Println("Controller starts up as BOOTSTRAP phase (per README.md)")

		// Use bootstrap controller which implements proper readiness checks
		bootstrapController := NewBootstrapController(c)
		clusterState, err := bootstrapController.ExecuteBootstrap(ctx)
		if err != nil {
			return nil, err
		}
		
		// Mark bootstrap as complete
		c.isBootstrap = false
		
		// Save controller state after successful bootstrap (only if leader)
		if err := c.saveControllerStateAfterBootstrap(ctx); err != nil {
			log.Printf("Warning: Failed to save controller state after bootstrap: %v", err)
		}
		
		// IMPORTANT: Re-query cluster state to get fresh replication configuration
		// Bootstrap just configured new roles, so we need to refresh our view
		log.Println("Bootstrap completed - re-querying cluster state to refresh internal view...")
		freshClusterState, err := c.discoverOperationalCluster(ctx)
		if err != nil {
			log.Printf("Warning: Failed to refresh cluster state after bootstrap: %v", err)
			log.Println("Returning bootstrap state as fallback")
			return clusterState, nil
		}
		
		log.Println("✅ Cluster state refreshed after bootstrap completion")
		return freshClusterState, nil
	}

	// Operational phase - existing logic
	log.Println("=== OPERATIONAL PHASE ===")
	clusterState, err := c.discoverOperationalCluster(ctx)
	if err != nil {
		return nil, err
	}

	// Ensure gateway is operational if cluster is stable
	if c.gatewayServer != nil && c.gatewayServer.IsBootstrapPhase() {
		// Check if cluster is in a healthy operational state
		if clusterState != nil && clusterState.StateType == OPERATIONAL_STATE && clusterState.CurrentMain != "" {
			log.Println("Cluster is in healthy operational state - transitioning gateway to operational phase")
			c.gatewayServer.SetBootstrapPhase(false)

			// Start gateway if not already started
			if err := c.gatewayServer.Start(ctx); err != nil {
				log.Printf("Warning: Failed to start gateway after transitioning to operational: %v", err)
			} else {
				log.Println("✅ Gateway started successfully after operational phase transition")
			}
		}
	}

	return clusterState, nil
}

// discoverOperationalCluster handles operational phase discovery (existing logic)
func (c *MemgraphController) discoverOperationalCluster(ctx context.Context) (*ClusterState, error) {
	log.Println("Discovering Memgraph cluster in operational phase...")

	clusterState, err := c.podDiscovery.DiscoverPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover pods: %w", err)
	}

	// Update connection pool with fresh pod IPs
	for _, podInfo := range clusterState.Pods {
		if podInfo.Pod != nil && podInfo.Pod.Status.PodIP != "" {
			c.memgraphClient.connectionPool.UpdatePodIP(podInfo.Name, podInfo.Pod.Status.PodIP)
		}
	}

	if len(clusterState.Pods) == 0 {
		log.Println("No pods found in cluster")
		return clusterState, nil
	}

	// CRITICAL: During OPERATIONAL phase, NEVER trigger bootstrap logic
	// Only controller.isBootstrap should determine bootstrap vs operational mode
	// This prevents inappropriate bootstrap classification during immediate failover
	if c.isBootstrap {
		log.Printf("Controller in bootstrap phase - will perform bootstrap validation")
		clusterState.IsBootstrapPhase = true
	} else {
		log.Printf("Controller in operational phase - maintaining OPERATIONAL_STATE authority")
		clusterState.IsBootstrapPhase = false
		clusterState.StateType = OPERATIONAL_STATE // Explicitly preserve OPERATIONAL state
		// Preserve target main index from controller state
		if c.targetMainIndex >= 0 {
			clusterState.TargetMainIndex = c.targetMainIndex
		}
	}

	clusterState.LastStateChange = time.Now()

	log.Printf("Discovered %d pods, starting bootstrap discovery...", len(clusterState.Pods))

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
		role, err := c.memgraphClient.QueryReplicationRoleWithRetry(ctx, podInfo.BoltAddress)
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

			replicasResp, err := c.memgraphClient.QueryReplicasWithRetry(ctx, podInfo.BoltAddress)
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

	// Update SYNC replica information based on actual main's replica data
	c.updateSyncReplicaInfo(clusterState)

	// Operational phase - detect and handle main failover scenarios  
	log.Println("Checking for main failover scenarios...")
	if c.detectMainFailover(clusterState) {
		log.Printf("Main failover detected - handling failover...")
		if err := c.handleMainFailover(ctx, clusterState); err != nil {
			log.Printf("⚠️  Failover handling failed: %v", err)
			// Don't crash - log warning and continue as per design
		}
	}
	
	// Set operational phase properties
	clusterState.IsBootstrapPhase = false
	clusterState.StateType = OPERATIONAL_STATE

	// Validate controller state consistency
	if warnings := clusterState.ValidateControllerState(c.config); len(warnings) > 0 {
		log.Printf("⚠️  Controller state validation warnings:")
		for _, warning := range warnings {
			log.Printf("  - %s", warning)
		}
	}

	// NOW select main based on actual Memgraph state (not pod labels)
	log.Println("Selecting main based on actual Memgraph replication state...")
	c.selectMainAfterQuerying(ctx, clusterState)

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

// performBootstrapValidation validates cluster state during bootstrap phase
func (c *MemgraphController) performBootstrapValidation(clusterState *ClusterState) error {
	// Classify the current cluster state
	oldStateType := clusterState.StateType
	stateType := clusterState.ClassifyClusterState(c.config)
	clusterState.StateType = stateType

	// Log state transition if changed
	clusterState.LogStateTransition(oldStateType, "bootstrap classification")

	log.Printf("Bootstrap validation: cluster state classified as %s", stateType.String())

	// Log pod role distribution for debugging
	mainPods := clusterState.GetMainPods()
	replicaPods := clusterState.GetReplicaPods()
	log.Printf("Pod role distribution: %d main pods %v, %d replica pods %v",
		len(mainPods), mainPods, len(replicaPods), replicaPods)


	// Check if bootstrap is safe to proceed
	isBootstrapSafe := clusterState.IsBootstrapSafe(c.config)
	clusterState.BootstrapSafe = isBootstrapSafe

	if !isBootstrapSafe {
		// UNKNOWN_STATE - controller should crash immediately per README.md
		log.Printf("❌ CONTROLLER CRASH: UNKNOWN_STATE detected")
		log.Printf("❌ Cluster is in an unknown state that requires manual intervention")
		log.Printf("🔧 Recovery: Human must fix the cluster before controller can start")
		return fmt.Errorf("controller crash: cluster in UNKNOWN_STATE, human intervention required")
	}

	// Safe states - proceed with bootstrap and determine target main index
	switch stateType {
	case INITIAL_STATE:
		log.Printf("✅ SAFE: Fresh cluster state detected")
		log.Printf("All pods are main or no role data yet - no data divergence risk")
		log.Printf("Will apply deterministic role assignment")

	case OPERATIONAL_STATE:
		log.Printf("✅ SAFE: Operational cluster state detected")
		log.Printf("Exactly one main found - will learn existing topology")
		log.Printf("Current main: %s", mainPods[0])
	}

	// Determine target main index (0 or 1)
	targetMainIndex, err := clusterState.DetermineMainIndex(c.config)
	if err != nil {
		return fmt.Errorf("failed to determine target main index: %w", err)
	}

	clusterState.TargetMainIndex = targetMainIndex
	log.Printf("Target main index determined: %d (pod: %s)",
		targetMainIndex, c.config.GetPodName(targetMainIndex))

	return nil
}


// selectMainAfterQuerying selects main based on actual Memgraph replication state
func (c *MemgraphController) selectMainAfterQuerying(ctx context.Context, clusterState *ClusterState) {
	// After bootstrap validation, we now have authority to make decisions
	clusterState.IsBootstrapPhase = false

	// Enhanced main selection using controller state authority
	log.Printf("Enhanced main selection: state=%s, target_index=%d",
		clusterState.StateType.String(), clusterState.TargetMainIndex)

	// Use controller state authority based on cluster state
	switch clusterState.StateType {
	case INITIAL_STATE:
		c.applyDeterministicRoles(clusterState)
	case OPERATIONAL_STATE:
		c.learnExistingTopology(clusterState)
	default:
		// For other states, apply enhanced main selection logic
		c.enhancedMainSelection(ctx, clusterState)
	}

	// Validate main selection result
	c.validateMainSelection(ctx, clusterState)

	// Log comprehensive cluster health summary
	healthSummary := clusterState.GetClusterHealthSummary()
	log.Printf("📋 CLUSTER HEALTH SUMMARY: %+v", healthSummary)
}

// applyDeterministicRoles applies roles for fresh/initial clusters
func (c *MemgraphController) applyDeterministicRoles(clusterState *ClusterState) {
	log.Printf("Applying deterministic role assignment for fresh cluster")

	// Use the determined target main index
	targetMainName := c.config.GetPodName(clusterState.TargetMainIndex)
	clusterState.CurrentMain = targetMainName

	log.Printf("Deterministic main assignment: %s (index %d)",
		targetMainName, clusterState.TargetMainIndex)

	// Log planned topology
	syncReplicaIndex := 1 - clusterState.TargetMainIndex // 0->1, 1->0
	syncReplicaName := c.config.GetPodName(syncReplicaIndex)

	log.Printf("Planned topology:")
	log.Printf("  Main: %s (index %d)", targetMainName, clusterState.TargetMainIndex)
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
func (c *MemgraphController) learnExistingTopology(clusterState *ClusterState) {
	log.Printf("Learning existing operational topology")

	mainPods := clusterState.GetMainPods()
	if len(mainPods) == 1 {
		currentMain := mainPods[0]
		clusterState.CurrentMain = currentMain
		log.Printf("Learned existing main: %s", currentMain)

		// Track last known main for operational phase detection
		c.lastKnownMain = currentMain

		// Notify gateway of main endpoint (async to avoid blocking reconciliation)
		go c.updateGatewayMain()

		// Extract current main index for tracking using consolidated method
		currentMainIndex := c.config.ExtractPodIndex(currentMain)
		if currentMainIndex >= 0 {
			if err := c.updateTargetMainIndex(context.Background(), clusterState, currentMainIndex,
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
		targetMainName := c.config.GetPodName(clusterState.TargetMainIndex)
		clusterState.CurrentMain = targetMainName
		log.Printf("Using determined target main as fallback: %s", targetMainName)
	}
}
