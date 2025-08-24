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

		podInfo := NewPodInfo(&pod, pd.config.ServiceName)
		clusterState.Pods[pod.Name] = podInfo

		log.Printf("Discovered pod: %s, IP: %s, Timestamp: %s",
			podInfo.Name,
			pod.Status.PodIP,
			podInfo.Timestamp.Format(time.RFC3339))
	}

	// Master selection will be done AFTER querying actual Memgraph state
	// This ensures we use real replication roles for decision making
	log.Printf("Pod discovery complete. Master selection deferred until after Memgraph querying.")

	return clusterState, nil
}

func (pd *PodDiscovery) selectMaster(clusterState *ClusterState) {
	// SYNC Replica Priority Strategy:
	// 1. Look for existing MAIN node (prefer current master if healthy)
	// 2. If no MAIN, look for SYNC replica (guaranteed consistency)
	// 3. If no SYNC replica, fall back to timestamp-based selection
	
	var currentMain *PodInfo
	var syncReplica *PodInfo
	var latestPod *PodInfo
	var latestTime time.Time

	// Analyze all pods for master selection
	for _, podInfo := range clusterState.Pods {
		// Priority 1: Existing MAIN node (prefer current master)
		if podInfo.MemgraphRole == "main" {
			currentMain = podInfo
			log.Printf("Found existing MAIN node: %s", podInfo.Name)
		}
		
		// Priority 2: SYNC replica (guaranteed data consistency)
		if podInfo.IsSyncReplica {
			syncReplica = podInfo
			log.Printf("Found SYNC replica: %s", podInfo.Name)
		}
		
		// Priority 3: Latest timestamp (fallback)
		if latestPod == nil || podInfo.Timestamp.After(latestTime) {
			latestPod = podInfo
			latestTime = podInfo.Timestamp
		}
	}

	// Master selection decision tree
	var selectedMaster *PodInfo
	var selectionReason string
	
	if currentMain != nil {
		// Prefer existing MAIN node (avoid unnecessary failover)
		selectedMaster = currentMain
		selectionReason = "existing MAIN node"
	} else if syncReplica != nil {
		// ONLY safe automatic promotion: SYNC replica has all committed data
		selectedMaster = syncReplica
		selectionReason = "SYNC replica (guaranteed consistency)"
		log.Printf("PROMOTING SYNC REPLICA: %s has all committed transactions", syncReplica.Name)
	} else {
		// CRITICAL: No SYNC replica available - DO NOT auto-promote ASYNC replicas
		// ASYNC replicas may be missing committed transactions, causing data loss
		if len(clusterState.Pods) > 1 {
			log.Printf("CRITICAL: No SYNC replica available for safe automatic promotion")
			log.Printf("CRITICAL: Cannot guarantee data consistency - manual intervention required")
			log.Printf("CRITICAL: ASYNC replicas may be missing committed transactions")
			
			// Do NOT select any master - require manual intervention
			selectedMaster = nil
			selectionReason = "no safe automatic promotion possible (SYNC replica unavailable)"
		} else {
			// Single pod scenario - safe to promote (no replication risk)
			selectedMaster = latestPod
			selectionReason = "single pod cluster (no replication consistency risk)"
		}
	}

	if selectedMaster != nil {
		clusterState.CurrentMaster = selectedMaster.Name
		log.Printf("Selected master: %s (reason: %s, timestamp: %s)",
			selectedMaster.Name,
			selectionReason,
			selectedMaster.Timestamp.Format(time.RFC3339))
	} else {
		log.Printf("No pods available for master selection")
	}
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
		podInfo := NewPodInfo(&pod, pd.config.ServiceName)
		clusterState.Pods[pod.Name] = podInfo
	}

	return clusterState, nil
}

// DiscoverCluster discovers the current state of the Memgraph cluster
func (c *MemgraphController) DiscoverCluster(ctx context.Context) (*ClusterState, error) {
	log.Println("Discovering Memgraph cluster...")
	
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

	// Determine if this is truly a bootstrap phase or operational reconciliation
	// Only mark as bootstrap if:
	// 1. Controller has no last known master (first run)
	// 2. Controller hasn't established operational state before
	isBootstrap := c.lastKnownMaster == "" && c.targetMasterIndex < 0
	
	if isBootstrap {
		log.Printf("First run detected - entering bootstrap phase for safety validation")
		clusterState.IsBootstrapPhase = true
	} else {
		log.Printf("Operational reconciliation - maintaining authority over cluster state")
		clusterState.IsBootstrapPhase = false
		// Preserve target master index from controller state
		if c.targetMasterIndex >= 0 {
			clusterState.TargetMasterIndex = c.targetMasterIndex
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

	// Update SYNC replica information based on actual master's replica data
	c.updateSyncReplicaInfo(clusterState)

	// Classify cluster state and perform bootstrap safety validation
	log.Println("Classifying cluster state for bootstrap safety...")
	if err := c.performBootstrapValidation(clusterState); err != nil {
		return nil, fmt.Errorf("bootstrap validation failed: %w", err)
	}
	
	// Validate controller state consistency
	if warnings := clusterState.ValidateControllerState(); len(warnings) > 0 {
		log.Printf("⚠️  Controller state validation warnings:")
		for _, warning := range warnings {
			log.Printf("  - %s", warning)
		}
	}

	// NOW select master based on actual Memgraph state (not pod labels)
	log.Println("Selecting master based on actual Memgraph replication state...")
	c.selectMasterAfterQuerying(clusterState)

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

// getPodNames returns a slice of pod names for logging purposes
func getPodNames(pods map[string]*PodInfo) []string {
	var names []string
	for name := range pods {
		names = append(names, name)
	}
	return names
}

// performBootstrapValidation validates cluster state during bootstrap phase
func (c *MemgraphController) performBootstrapValidation(clusterState *ClusterState) error {
	// Classify the current cluster state
	oldStateType := clusterState.StateType
	stateType := clusterState.ClassifyClusterState()
	clusterState.StateType = stateType
	
	// Log state transition if changed
	clusterState.LogStateTransition(oldStateType, "bootstrap classification")
	
	log.Printf("Bootstrap validation: cluster state classified as %s", stateType.String())
	
	// Log pod role distribution for debugging
	mainPods := clusterState.GetMainPods()
	replicaPods := clusterState.GetReplicaPods()
	log.Printf("Pod role distribution: %d main pods %v, %d replica pods %v", 
		len(mainPods), mainPods, len(replicaPods), replicaPods)
	
	// Check if this is controller startup (bootstrap) or operational reconciliation
	isControllerStartup := c.lastKnownMaster == ""
	
	// Check if bootstrap is safe to proceed
	isBootstrapSafe := clusterState.IsBootstrapSafe()
	clusterState.BootstrapSafe = isBootstrapSafe
	
	if !isBootstrapSafe {
		// DANGEROUS states during bootstrap - refuse to continue
		switch stateType {
		case MIXED_STATE:
			log.Printf("❌ BOOTSTRAP BLOCKED: Mixed replication state detected")
			log.Printf("❌ Some pods are main, some are replica - unclear data freshness")
			log.Printf("❌ Manual intervention required to determine safe master")
			log.Printf("❌ Possible data divergence between pods")
			log.Printf("🔧 Recovery options:")
			log.Printf("  1. Check which pod has latest data using mgconsole")
			log.Printf("  2. Manually set desired master to MAIN role")
			log.Printf("  3. Set all other pods to REPLICA role")
			log.Printf("  4. Restart controller after manual intervention")
			return fmt.Errorf("unsafe mixed state during bootstrap: main=%v, replica=%v", mainPods, replicaPods)
			
		case NO_MASTER_STATE:
			if isControllerStartup {
				log.Printf("❌ BOOTSTRAP BLOCKED: No master found, unclear data freshness")
				log.Printf("❌ All pods are replicas - cannot determine which has latest data")
				log.Printf("❌ Manual intervention required to select master")
				log.Printf("🔧 Recovery options:")
				log.Printf("  1. Identify pod with latest data (check STORAGE INFO)")
				log.Printf("  2. Promote chosen pod: kubectl exec <pod> -- mgconsole -e 'SET REPLICATION ROLE TO MAIN;'")
				log.Printf("  3. Restart controller after manual promotion")
				return fmt.Errorf("no master during bootstrap: all %d pods are replicas", len(replicaPods))
			} else {
				log.Printf("🚨 OPERATIONAL: Master failure detected - promoting SYNC replica")
				return c.handleMasterFailurePromotion(clusterState, replicaPods)
			}
			
		case SPLIT_BRAIN_STATE:
			if isControllerStartup {
				log.Printf("❌ BOOTSTRAP BLOCKED: Multiple masters detected")
				log.Printf("❌ Split-brain condition - potential data divergence")
				log.Printf("❌ Manual intervention required to resolve conflicts")
				log.Printf("🔧 Recovery options:")
				log.Printf("  1. Compare data between masters using STORAGE INFO")
				log.Printf("  2. Choose master with most recent data")
				log.Printf("  3. Demote others: kubectl exec <pod> -- mgconsole -e 'SET REPLICATION ROLE TO REPLICA WITH PORT 10000;'")
				log.Printf("  4. Restart controller after resolving split-brain")
				return fmt.Errorf("split-brain during bootstrap: multiple masters %v", mainPods)
			} else {
				log.Printf("🔄 OPERATIONAL: Split-brain detected - enforcing current master authority")
				return c.enforceMasterAuthority(clusterState, mainPods)
			}
			
		default:
			log.Printf("❌ BOOTSTRAP BLOCKED: Unknown unsafe state")
			return fmt.Errorf("unknown unsafe cluster state: %s", stateType.String())
		}
	}
	
	// Safe states - proceed with bootstrap and determine target master index
	switch stateType {
	case INITIAL_STATE:
		log.Printf("✅ SAFE: Fresh cluster state detected")
		log.Printf("All pods are main or no role data yet - no data divergence risk")
		log.Printf("Will apply deterministic role assignment")
		
	case OPERATIONAL_STATE:
		log.Printf("✅ SAFE: Operational cluster state detected")
		log.Printf("Exactly one master found - will learn existing topology")
		log.Printf("Current master: %s", mainPods[0])
	}
	
	// Determine target master index (0 or 1)
	targetMasterIndex, err := clusterState.DetermineMasterIndex(c.config)
	if err != nil {
		return fmt.Errorf("failed to determine target master index: %w", err)
	}
	
	clusterState.TargetMasterIndex = targetMasterIndex
	log.Printf("Target master index determined: %d (pod: %s)", 
		targetMasterIndex, c.config.GetPodName(targetMasterIndex))
	
	return nil
}

// selectMasterAfterQuerying selects master based on actual Memgraph replication state
func (c *MemgraphController) selectMasterAfterQuerying(clusterState *ClusterState) {
	// After bootstrap validation, we now have authority to make decisions
	clusterState.IsBootstrapPhase = false
	
	// Enhanced master selection using controller state authority
	log.Printf("Enhanced master selection: state=%s, target_index=%d", 
		clusterState.StateType.String(), clusterState.TargetMasterIndex)
	
	// Use controller state authority based on cluster state
	switch clusterState.StateType {
	case INITIAL_STATE:
		c.applyDeterministicRoles(clusterState)
	case OPERATIONAL_STATE:
		c.learnExistingTopology(clusterState)
	default:
		// For other states, apply enhanced master selection logic
		c.enhancedMasterSelection(clusterState)
	}
	
	// Validate master selection result
	c.validateMasterSelection(clusterState)
	
	// Log comprehensive cluster health summary
	healthSummary := clusterState.GetClusterHealthSummary()
	log.Printf("📋 CLUSTER HEALTH SUMMARY: %+v", healthSummary)
}

// applyDeterministicRoles applies roles for fresh/initial clusters
func (c *MemgraphController) applyDeterministicRoles(clusterState *ClusterState) {
	log.Printf("Applying deterministic role assignment for fresh cluster")
	
	// Use the determined target master index
	targetMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	clusterState.CurrentMaster = targetMasterName
	
	log.Printf("Deterministic master assignment: %s (index %d)", 
		targetMasterName, clusterState.TargetMasterIndex)
	
	// Log planned topology
	syncReplicaIndex := 1 - clusterState.TargetMasterIndex // 0->1, 1->0
	syncReplicaName := c.config.GetPodName(syncReplicaIndex)
	
	log.Printf("Planned topology:")
	log.Printf("  Master: %s (index %d)", targetMasterName, clusterState.TargetMasterIndex)
	log.Printf("  SYNC replica: %s (index %d)", syncReplicaName, syncReplicaIndex)
	
	// Mark remaining pods as ASYNC replicas
	asyncCount := 0
	for podName := range clusterState.Pods {
		if podName != targetMasterName && podName != syncReplicaName {
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
		currentMaster := mainPods[0]
		clusterState.CurrentMaster = currentMaster
		log.Printf("Learned existing master: %s", currentMaster)
		
		// Track last known master for operational phase detection
		c.lastKnownMaster = currentMaster
		
		// Notify gateway of master endpoint (async to avoid blocking reconciliation)
		go c.updateGatewayMaster()
		
		// Extract current master index for tracking
		currentMasterIndex := c.config.ExtractPodIndex(currentMaster)
		if currentMasterIndex >= 0 {
			clusterState.TargetMasterIndex = currentMasterIndex
			c.targetMasterIndex = currentMasterIndex
			log.Printf("Updated target master index to match existing: %d", currentMasterIndex)
		}
		
		// Log current SYNC replica
		for podName, podInfo := range clusterState.Pods {
			if podInfo.IsSyncReplica {
				log.Printf("Current SYNC replica: %s", podName)
				break
			}
		}
		
	} else {
		log.Printf("WARNING: Expected exactly 1 master in operational state, found %d: %v", 
			len(mainPods), mainPods)
		
		// Use the determined target master as fallback
		targetMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
		clusterState.CurrentMaster = targetMasterName
		log.Printf("Using determined target master as fallback: %s", targetMasterName)
	}
}

