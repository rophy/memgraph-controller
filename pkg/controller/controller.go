package controller

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// reconcileRequest represents a request to reconcile the cluster
type reconcileRequest struct {
	reason    string
	timestamp time.Time
}

type MemgraphController struct {
	clientset       kubernetes.Interface
	config          *Config
	podDiscovery    *PodDiscovery
	memgraphClient  *MemgraphClient
	httpServer      *HTTPServer
	
	// Controller loop state
	isRunning       bool
	mu              sync.RWMutex
	lastReconcile   time.Time
	failureCount    int
	maxFailures     int
	
	// Event-driven reconciliation
	podInformer     cache.SharedInformer
	informerFactory informers.SharedInformerFactory
	workQueue       chan reconcileRequest
	stopCh          chan struct{}
	
	// Reconciliation metrics
	metrics         *ReconciliationMetrics
}

func NewMemgraphController(clientset kubernetes.Interface, config *Config) *MemgraphController {
	controller := &MemgraphController{
		clientset:      clientset,
		config:         config,
		podDiscovery:   NewPodDiscovery(clientset, config),
		memgraphClient: NewMemgraphClient(config),
	}
	
	// Initialize HTTP server
	controller.httpServer = NewHTTPServer(controller, config)
	
	// Initialize controller state
	controller.maxFailures = 5
	controller.workQueue = make(chan reconcileRequest, 100)
	controller.stopCh = make(chan struct{})
	
	// Initialize reconciliation metrics
	controller.metrics = &ReconciliationMetrics{}
	
	// Set up pod informer for event-driven reconciliation
	controller.setupInformers()
	
	return controller
}

func (c *MemgraphController) TestConnection() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pods, err := c.clientset.CoreV1().Pods(c.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + c.config.AppName,
	})
	if err != nil {
		return err
	}

	log.Printf("Successfully connected to Kubernetes API. Found %d pods with app.kubernetes.io/name=%s in namespace %s",
		len(pods.Items), c.config.AppName, c.config.Namespace)

	return nil
}

func (c *MemgraphController) DiscoverCluster(ctx context.Context) (*ClusterState, error) {
	log.Println("Discovering Memgraph cluster...")
	
	clusterState, err := c.podDiscovery.DiscoverPods(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover pods: %w", err)
	}

	if len(clusterState.Pods) == 0 {
		log.Println("No pods found in cluster")
		return clusterState, nil
	}

	// Mark this as bootstrap phase for safety validation
	clusterState.IsBootstrapPhase = true
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
			log.Printf("❌ BOOTSTRAP BLOCKED: No master found, unclear data freshness")
			log.Printf("❌ All pods are replicas - cannot determine which has latest data")
			log.Printf("❌ Manual intervention required to select master")
			log.Printf("🔧 Recovery options:")
			log.Printf("  1. Identify pod with latest data (check STORAGE INFO)")
			log.Printf("  2. Promote chosen pod: kubectl exec <pod> -- mgconsole -e 'SET REPLICATION ROLE TO MAIN;'")
			log.Printf("  3. Restart controller after manual promotion")
			return fmt.Errorf("no master during bootstrap: all %d pods are replicas", len(replicaPods))
			
		case SPLIT_BRAIN_STATE:
			log.Printf("❌ BOOTSTRAP BLOCKED: Multiple masters detected")
			log.Printf("❌ Split-brain condition - potential data divergence")
			log.Printf("❌ Manual intervention required to resolve conflicts")
			log.Printf("🔧 Recovery options:")
			log.Printf("  1. Compare data between masters using STORAGE INFO")
			log.Printf("  2. Choose master with most recent data")
			log.Printf("  3. Demote others: kubectl exec <pod> -- mgconsole -e 'SET REPLICATION ROLE TO REPLICA;'")
			log.Printf("  4. Restart controller after resolving split-brain")
			return fmt.Errorf("split-brain during bootstrap: multiple masters %v", mainPods)
			
		default:
			log.Printf("❌ BOOTSTRAP BLOCKED: Unknown unsafe state")
			return fmt.Errorf("unknown unsafe cluster state: %s", stateType.String())
		}
	}
	
	// Safe states - proceed with bootstrap and determine target master index
	switch stateType {
	case INITIAL_STATE:
		log.Printf("✅ Bootstrap SAFE: Fresh cluster state detected")
		log.Printf("All pods are main or no role data yet - no data divergence risk")
		log.Printf("Will apply deterministic role assignment")
		
	case OPERATIONAL_STATE:
		log.Printf("✅ Bootstrap SAFE: Operational cluster state detected")
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
		
		// Extract current master index for tracking
		currentMasterIndex := c.config.ExtractPodIndex(currentMaster)
		if currentMasterIndex >= 0 {
			clusterState.TargetMasterIndex = currentMasterIndex
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

// enhancedMasterSelection applies enhanced master selection with SYNC replica priority
func (c *MemgraphController) enhancedMasterSelection(clusterState *ClusterState) {
	log.Printf("Applying enhanced master selection logic...")
	
	// Priority-based master selection strategy:
	// 1. Controller's target master (if available and healthy)
	// 2. Existing MAIN node (avoid unnecessary failover)
	// 3. SYNC replica (guaranteed data consistency)
	// 4. Manual intervention required (no safe automatic promotion)
	
	targetMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	var targetMaster *PodInfo
	var existingMain *PodInfo
	var syncReplica *PodInfo
	
	// Analyze available pods
	for podName, podInfo := range clusterState.Pods {
		// Priority 1: Controller's target master
		if podName == targetMasterName {
			targetMaster = podInfo
		}
		
		// Priority 2: Existing MAIN node
		if podInfo.MemgraphRole == "main" {
			existingMain = podInfo
		}
		
		// Priority 3: SYNC replica
		if podInfo.IsSyncReplica {
			syncReplica = podInfo
		}
	}
	
	// Enhanced master selection decision tree
	var selectedMaster *PodInfo
	var selectionReason string
	
	// Priority 1: Controller's target master (if healthy)
	if targetMaster != nil && c.isPodHealthyForMaster(targetMaster) {
		selectedMaster = targetMaster
		selectionReason = fmt.Sprintf("controller target master (index %d)", clusterState.TargetMasterIndex)
		log.Printf("✅ Using controller's target master: %s", targetMasterName)
		
	// Priority 2: Existing MAIN node (avoid unnecessary failover)
	} else if existingMain != nil && c.isPodHealthyForMaster(existingMain) {
		selectedMaster = existingMain
		selectionReason = "existing MAIN node (avoid failover)"
		log.Printf("✅ Keeping existing MAIN node: %s", existingMain.Name)
		
		// Update target master index to match existing master
		existingMasterIndex := c.config.ExtractPodIndex(existingMain.Name)
		if existingMasterIndex >= 0 && existingMasterIndex <= 1 {
			clusterState.TargetMasterIndex = existingMasterIndex
			log.Printf("Updated target master index to match existing: %d", existingMasterIndex)
		}
		
	// Priority 3: SYNC replica (guaranteed data consistency)
	} else if syncReplica != nil && c.isPodHealthyForMaster(syncReplica) {
		selectedMaster = syncReplica
		selectionReason = "SYNC replica promotion (guaranteed consistency)"
		log.Printf("🔄 PROMOTING SYNC REPLICA: %s has all committed transactions", syncReplica.Name)
		
		// Update target master index to match SYNC replica
		syncReplicaIndex := c.config.ExtractPodIndex(syncReplica.Name)
		if syncReplicaIndex >= 0 && syncReplicaIndex <= 1 {
			clusterState.TargetMasterIndex = syncReplicaIndex
			log.Printf("Updated target master index to match SYNC replica: %d", syncReplicaIndex)
		}
		
	// Priority 4: No safe automatic promotion available
	} else {
		c.handleNoSafePromotion(clusterState, targetMasterName, existingMain, syncReplica)
		return
	}
	
	// Set the selected master
	if selectedMaster != nil {
		clusterState.CurrentMaster = selectedMaster.Name
		log.Printf("✅ Enhanced master selection result: %s (reason: %s)", 
			selectedMaster.Name, selectionReason)
	}
	
	// Create and log master selection metrics
	metrics := &MasterSelectionMetrics{
		Timestamp:            time.Now(),
		StateType:            clusterState.StateType,
		TargetMasterIndex:    clusterState.TargetMasterIndex,
		SelectedMaster:       clusterState.CurrentMaster,
		SelectionReason:      selectionReason,
		HealthyPodsCount:     c.countHealthyPods(clusterState),
		SyncReplicaAvailable: syncReplica != nil,
		FailoverDetected:     false,
		DecisionFactors:      c.buildDecisionFactors(targetMaster, existingMain, syncReplica),
	}
	
	clusterState.LogMasterSelectionDecision(metrics)
}

// countHealthyPods counts pods that are healthy for master role
func (c *MemgraphController) countHealthyPods(clusterState *ClusterState) int {
	count := 0
	for _, podInfo := range clusterState.Pods {
		if c.isPodHealthyForMaster(podInfo) {
			count++
		}
	}
	return count
}

// buildDecisionFactors creates list of factors that influenced master selection
func (c *MemgraphController) buildDecisionFactors(targetMaster, existingMain, syncReplica *PodInfo) []string {
	var factors []string
	
	if targetMaster != nil {
		if c.isPodHealthyForMaster(targetMaster) {
			factors = append(factors, "target_master_healthy")
		} else {
			factors = append(factors, "target_master_unhealthy")
		}
	} else {
		factors = append(factors, "target_master_missing")
	}
	
	if existingMain != nil {
		if c.isPodHealthyForMaster(existingMain) {
			factors = append(factors, "existing_main_healthy")
		} else {
			factors = append(factors, "existing_main_unhealthy")
		}
	} else {
		factors = append(factors, "no_existing_main")
	}
	
	if syncReplica != nil {
		if c.isPodHealthyForMaster(syncReplica) {
			factors = append(factors, "sync_replica_available")
		} else {
			factors = append(factors, "sync_replica_unhealthy")
		}
	} else {
		factors = append(factors, "no_sync_replica")
	}
	
	return factors
}

// isPodHealthyForMaster checks if a pod is healthy enough to be master
func (c *MemgraphController) isPodHealthyForMaster(podInfo *PodInfo) bool {
	// Pod must have a bolt address (IP assigned)
	if podInfo.BoltAddress == "" {
		log.Printf("Pod %s not healthy for master: no bolt address", podInfo.Name)
		return false
	}
	
	// Pod must have been successfully queried for Memgraph role
	if podInfo.MemgraphRole == "" {
		log.Printf("Pod %s not healthy for master: no Memgraph role info", podInfo.Name)
		return false
	}
	
	return true
}

// handleNoSafePromotion handles scenarios where no safe automatic promotion is possible
func (c *MemgraphController) handleNoSafePromotion(clusterState *ClusterState, targetMasterName string, existingMain, syncReplica *PodInfo) {
	log.Printf("❌ CRITICAL: No safe automatic master promotion available")
	
	// Analyze why promotion is not safe
	if targetMaster, exists := clusterState.Pods[targetMasterName]; exists {
		if !c.isPodHealthyForMaster(targetMaster) {
			log.Printf("Target master %s is not healthy", targetMasterName)
		}
	} else {
		log.Printf("Target master %s not found in cluster", targetMasterName)
	}
	
	if existingMain != nil && !c.isPodHealthyForMaster(existingMain) {
		log.Printf("Existing main %s is not healthy", existingMain.Name)
	}
	
	if syncReplica != nil && !c.isPodHealthyForMaster(syncReplica) {
		log.Printf("SYNC replica %s is not healthy", syncReplica.Name)
	} else if syncReplica == nil {
		log.Printf("No SYNC replica available for safe promotion")
	}
	
	// Provide recovery guidance
	log.Printf("🔧 Manual intervention required:")
	log.Printf("  1. Check pod health: kubectl get pods -n %s", c.config.Namespace)
	log.Printf("  2. Check Memgraph connectivity to pods")
	log.Printf("  3. Manually promote healthy pod if available")
	log.Printf("  4. Consider emergency ASYNC→SYNC promotion if needed")
	
	// Do not set any master - require manual intervention
	clusterState.CurrentMaster = ""
}

// validateMasterSelection validates the master selection result
func (c *MemgraphController) validateMasterSelection(clusterState *ClusterState) {
	log.Printf("Validating master selection result...")
	
	if clusterState.CurrentMaster == "" {
		log.Printf("⚠️  No master selected - cluster will require manual intervention")
		return
	}
	
	// Validate selected master exists in pods
	masterPod, exists := clusterState.Pods[clusterState.CurrentMaster]
	if !exists {
		log.Printf("❌ VALIDATION FAILED: Selected master %s not found in pods", clusterState.CurrentMaster)
		clusterState.CurrentMaster = ""
		return
	}
	
	// Validate master health
	if !c.isPodHealthyForMaster(masterPod) {
		log.Printf("❌ VALIDATION FAILED: Selected master %s is not healthy", clusterState.CurrentMaster)
		clusterState.CurrentMaster = ""
		return
	}
	
	// Validate master index consistency
	masterIndex := c.config.ExtractPodIndex(clusterState.CurrentMaster)
	if masterIndex != clusterState.TargetMasterIndex {
		log.Printf("⚠️  Master index mismatch: selected=%d, target=%d", masterIndex, clusterState.TargetMasterIndex)
		if masterIndex >= 0 && masterIndex <= 1 {
			// Update target to match reality
			clusterState.TargetMasterIndex = masterIndex
			log.Printf("Updated target master index to match selection: %d", masterIndex)
		}
	}
	
	log.Printf("✅ Master selection validation passed: %s (index %d)", 
		clusterState.CurrentMaster, clusterState.TargetMasterIndex)
}

// detectMasterFailover detects if current master has failed and promotion is needed
func (c *MemgraphController) detectMasterFailover(clusterState *ClusterState) bool {
	if clusterState.CurrentMaster == "" {
		// No master currently set - not a failover scenario
		return false
	}
	
	// Check if current master pod still exists
	masterPod, exists := clusterState.Pods[clusterState.CurrentMaster]
	if !exists {
		log.Printf("🚨 MASTER FAILOVER DETECTED: Master pod %s no longer exists", clusterState.CurrentMaster)
		return true
	}
	
	// Check if current master is still healthy
	if !c.isPodHealthyForMaster(masterPod) {
		log.Printf("🚨 MASTER FAILOVER DETECTED: Master pod %s is no longer healthy", clusterState.CurrentMaster)
		return true
	}
	
	// Check if current master still has MAIN role
	if masterPod.MemgraphRole != "main" {
		log.Printf("🚨 MASTER FAILOVER DETECTED: Master pod %s no longer has MAIN role (current: %s)", 
			clusterState.CurrentMaster, masterPod.MemgraphRole)
		return true
	}
	
	return false
}

// handleMasterFailover handles master failover scenarios with SYNC replica priority
func (c *MemgraphController) handleMasterFailover(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("🔄 Handling master failover...")
	
	// Clear failed master
	oldMaster := clusterState.CurrentMaster
	clusterState.CurrentMaster = ""
	
	// Find suitable replacement using SYNC replica priority
	var syncReplica *PodInfo
	var healthyReplicas []*PodInfo
	
	for _, podInfo := range clusterState.Pods {
		if podInfo.Name == oldMaster {
			continue // Skip the failed master
		}
		
		if c.isPodHealthyForMaster(podInfo) {
			healthyReplicas = append(healthyReplicas, podInfo)
			
			// Identify SYNC replica
			if podInfo.IsSyncReplica {
				syncReplica = podInfo
			}
		}
	}
	
	// Failover decision tree
	var newMaster *PodInfo
	var promotionReason string
	
	if syncReplica != nil {
		// Priority 1: SYNC replica (guaranteed data consistency)
		newMaster = syncReplica
		promotionReason = "SYNC replica failover (zero data loss)"
		log.Printf("✅ SYNC REPLICA FAILOVER: Promoting %s (guaranteed all committed data)", syncReplica.Name)
		
	} else if len(healthyReplicas) > 0 {
		// Priority 2: Healthy replica (potential data loss warning)
		newMaster = c.selectBestAsyncReplica(healthyReplicas, clusterState.TargetMasterIndex)
		promotionReason = "ASYNC replica failover (potential data loss)"
		log.Printf("⚠️  ASYNC REPLICA FAILOVER: Promoting %s (may have missing transactions)", newMaster.Name)
		log.Printf("⚠️  WARNING: Potential data loss - ASYNC replica may not have latest committed data")
		
	} else {
		// No healthy replicas available
		log.Printf("❌ CRITICAL: No healthy replicas available for failover")
		log.Printf("Cluster will remain without master until manual intervention")
		return fmt.Errorf("no healthy replicas available for master failover")
	}
	
	// Promote new master
	if newMaster != nil {
		log.Printf("🔄 Promoting new master: %s → %s (reason: %s)", 
			oldMaster, newMaster.Name, promotionReason)
		
		// Perform promotion
		err := c.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, newMaster.BoltAddress)
		if err != nil {
			return fmt.Errorf("failed to promote new master %s: %w", newMaster.Name, err)
		}
		
		// Update cluster state
		clusterState.CurrentMaster = newMaster.Name
		
		// Update target master index to match promoted master
		newMasterIndex := c.config.ExtractPodIndex(newMaster.Name)
		if newMasterIndex >= 0 && newMasterIndex <= 1 {
			clusterState.TargetMasterIndex = newMasterIndex
		}
		
		log.Printf("✅ Master failover completed: %s promoted successfully", newMaster.Name)
		
		// Log failover event with detailed metrics
		failoverMetrics := &MasterSelectionMetrics{
			Timestamp:            time.Now(),
			StateType:            clusterState.StateType,
			TargetMasterIndex:    clusterState.TargetMasterIndex,
			SelectedMaster:       newMaster.Name,
			SelectionReason:      promotionReason,
			HealthyPodsCount:     len(healthyReplicas),
			SyncReplicaAvailable: syncReplica != nil,
			FailoverDetected:     true,
			DecisionFactors:      []string{fmt.Sprintf("old_master_failed:%s", oldMaster), fmt.Sprintf("data_safety:%t", syncReplica != nil)},
		}
		
		log.Printf("📊 FAILOVER EVENT: old_master=%s, new_master=%s, reason=%s, data_safe=%t", 
			oldMaster, newMaster.Name, promotionReason, syncReplica != nil)
		
		clusterState.LogMasterSelectionDecision(failoverMetrics)
	}
	
	return nil
}

// selectBestAsyncReplica selects the best ASYNC replica for failover
func (c *MemgraphController) selectBestAsyncReplica(replicas []*PodInfo, targetIndex int) *PodInfo {
	// Prefer replica that matches target master index
	targetMasterName := c.config.GetPodName(targetIndex)
	for _, replica := range replicas {
		if replica.Name == targetMasterName {
			log.Printf("Selected ASYNC replica matching target index: %s", replica.Name)
			return replica
		}
	}
	
	// Fallback: select replica with lowest index (deterministic)
	var bestReplica *PodInfo
	bestIndex := 999
	
	for _, replica := range replicas {
		replicaIndex := c.config.ExtractPodIndex(replica.Name)
		if replicaIndex >= 0 && replicaIndex < bestIndex {
			bestIndex = replicaIndex
			bestReplica = replica
		}
	}
	
	if bestReplica != nil {
		log.Printf("Selected ASYNC replica with lowest index: %s (index %d)", bestReplica.Name, bestIndex)
	}
	
	return bestReplica
}

// enforceExpectedTopology enforces controller's known good topology against state drift
func (c *MemgraphController) enforceExpectedTopology(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Enforcing expected topology against state drift...")
	
	// Reclassify current state to detect drift
	currentStateType := clusterState.ClassifyClusterState()
	
	// Check for problematic states that need correction
	switch currentStateType {
	case SPLIT_BRAIN_STATE:
		log.Printf("Split-brain detected during operational phase - applying resolution")
		return c.resolveSplitBrain(ctx, clusterState)
		
	case MIXED_STATE:
		log.Printf("Mixed state detected during operational phase - enforcing known topology")
		return c.enforceKnownTopology(ctx, clusterState)
		
	case NO_MASTER_STATE:
		log.Printf("No master detected - promoting expected master")
		return c.promoteExpectedMaster(ctx, clusterState)
		
	case OPERATIONAL_STATE:
		log.Printf("Cluster in healthy operational state")
		return nil
		
	case INITIAL_STATE:
		log.Printf("Cluster reverted to initial state - reapplying deterministic roles")
		c.applyDeterministicRoles(clusterState)
		return nil
		
	default:
		log.Printf("Unknown cluster state during operational phase: %s", currentStateType.String())
		return nil
	}
}

// resolveSplitBrain resolves split-brain scenarios using lower-index precedence
func (c *MemgraphController) resolveSplitBrain(ctx context.Context, clusterState *ClusterState) error {
	mainPods := clusterState.GetMainPods()
	log.Printf("Resolving split-brain: multiple masters detected: %v", mainPods)
	
	// Apply lower-index precedence rule
	expectedMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	
	// Demote all masters except the expected one
	var demotionErrors []error
	for _, masterPod := range mainPods {
		if masterPod != expectedMasterName {
			log.Printf("Demoting incorrect master: %s (expected: %s)", masterPod, expectedMasterName)
			
			if podInfo, exists := clusterState.Pods[masterPod]; exists {
				err := c.memgraphClient.SetReplicationRoleToReplicaWithRetry(ctx, podInfo.BoltAddress)
				if err != nil {
					log.Printf("Failed to demote incorrect master %s: %v", masterPod, err)
					demotionErrors = append(demotionErrors, fmt.Errorf("demote %s: %w", masterPod, err))
				} else {
					log.Printf("Successfully demoted incorrect master: %s", masterPod)
				}
			}
		}
	}
	
	if len(demotionErrors) > 0 {
		return fmt.Errorf("split-brain resolution had %d errors: %v", len(demotionErrors), demotionErrors)
	}
	
	log.Printf("Split-brain resolved: %s remains as master", expectedMasterName)
	return nil
}

// enforceKnownTopology enforces the controller's known topology
func (c *MemgraphController) enforceKnownTopology(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Enforcing known topology: target master index %d", clusterState.TargetMasterIndex)
	
	expectedMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	clusterState.CurrentMaster = expectedMasterName
	
	log.Printf("Enforcing expected master: %s", expectedMasterName)
	return nil
}

// promoteExpectedMaster promotes the expected master when no masters exist
func (c *MemgraphController) promoteExpectedMaster(ctx context.Context, clusterState *ClusterState) error {
	expectedMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	
	log.Printf("Promoting expected master: %s", expectedMasterName)
	
	if podInfo, exists := clusterState.Pods[expectedMasterName]; exists {
		err := c.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, podInfo.BoltAddress)
		if err != nil {
			return fmt.Errorf("failed to promote expected master %s: %w", expectedMasterName, err)
		}
		
		clusterState.CurrentMaster = expectedMasterName
		log.Printf("Successfully promoted expected master: %s", expectedMasterName)
		return nil
	}
	
	return fmt.Errorf("expected master pod %s not found", expectedMasterName)
}

// updateSyncReplicaInfo updates IsSyncReplica field for all pods based on actual master replica data
func (c *MemgraphController) updateSyncReplicaInfo(clusterState *ClusterState) {
	// Find current MAIN node
	var masterPod *PodInfo
	for _, podInfo := range clusterState.Pods {
		if podInfo.MemgraphRole == "main" {
			masterPod = podInfo
			break
		}
	}
	
	if masterPod == nil {
		// No MAIN node found, clear all SYNC replica flags
		for _, podInfo := range clusterState.Pods {
			podInfo.IsSyncReplica = false
		}
		return
	}
	
	// Mark all replicas as ASYNC first
	for _, podInfo := range clusterState.Pods {
		podInfo.IsSyncReplica = false
	}
	
	// Identify SYNC replicas from master's replica information
	for _, replica := range masterPod.ReplicasInfo {
		if replica.SyncMode == "sync" { // Memgraph returns lowercase "sync"
			// Convert replica name back to pod name (underscores to dashes)
			podName := strings.ReplaceAll(replica.Name, "_", "-")
			if podInfo, exists := clusterState.Pods[podName]; exists {
				podInfo.IsSyncReplica = true
				log.Printf("Identified SYNC replica: %s", podName)
			}
		}
	}
}

func (c *MemgraphController) TestMemgraphConnections(ctx context.Context) error {
	clusterState, err := c.podDiscovery.DiscoverPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover pods for connection testing: %w", err)
	}

	if len(clusterState.Pods) == 0 {
		log.Println("No pods found for connection testing")
		return nil
	}

	log.Printf("Testing Memgraph connections for %d pods...", len(clusterState.Pods))

	var connectionErrors []error
	successCount := 0

	for podName, podInfo := range clusterState.Pods {
		if podInfo.BoltAddress == "" {
			log.Printf("Skipping pod %s: no Bolt address", podName)
			continue
		}

		log.Printf("Testing connection to pod %s at %s", podName, podInfo.BoltAddress)
		
		// Use enhanced connection testing with retry
		err := c.memgraphClient.TestConnectionWithRetry(ctx, podInfo.BoltAddress)
		if err != nil {
			log.Printf("Failed to connect to pod %s: %v", podName, err)
			connectionErrors = append(connectionErrors, fmt.Errorf("pod %s: %w", podName, err))
			continue
		}

		log.Printf("Successfully connected to pod %s", podName)
		successCount++
	}

	// Log connection test summary
	log.Printf("Connection testing complete: %d/%d pods connected successfully", 
		successCount, len(clusterState.Pods))

	if len(connectionErrors) > 0 {
		log.Printf("Encountered %d connection errors:", len(connectionErrors))
		for _, err := range connectionErrors {
			log.Printf("  - %v", err)
		}
		// Don't return error for partial failures - let caller decide
	}

	return nil
}

// Reconcile performs a full reconciliation cycle
func (c *MemgraphController) Reconcile(ctx context.Context) error {
	log.Println("Starting reconciliation cycle...")
	
	// Discover the current cluster state
	clusterState, err := c.DiscoverCluster(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover cluster state: %w", err)
	}
	
	if len(clusterState.Pods) == 0 {
		log.Println("No Memgraph pods found in cluster")
		return nil
	}
	
	log.Printf("Current cluster state discovered:")
	log.Printf("  - Total pods: %d", len(clusterState.Pods))
	log.Printf("  - Current master: %s", clusterState.CurrentMaster)
	log.Printf("  - State type: %s", clusterState.StateType.String())
	log.Printf("  - Target master index: %d", clusterState.TargetMasterIndex)
	
	// Log pod states
	for podName, podInfo := range clusterState.Pods {
		log.Printf("  - Pod %s: State=%s, MemgraphRole=%s, Replicas=%d", 
			podName, podInfo.State, podInfo.MemgraphRole, len(podInfo.Replicas))
	}

	// Operational phase: detect failover and enforce expected topology
	if !clusterState.IsBootstrapPhase {
		// Check for master failover first
		if c.detectMasterFailover(clusterState) {
			log.Printf("Master failover detected - initiating failover procedure...")
			if err := c.handleMasterFailover(ctx, clusterState); err != nil {
				log.Printf("❌ Master failover failed: %v", err)
				// Continue with topology enforcement to handle partial failures
			}
		}
		
		// Enforce expected topology against drift
		if err := c.enforceExpectedTopology(ctx, clusterState); err != nil {
			return fmt.Errorf("failed to enforce expected topology: %w", err)
		}
	}

	// Configure replication if needed
	if err := c.ConfigureReplication(ctx, clusterState); err != nil {
		return fmt.Errorf("failed to configure replication: %w", err)
	}
	
	// Sync pod labels with replication state
	if err := c.SyncPodLabels(ctx, clusterState); err != nil {
		return fmt.Errorf("failed to sync pod labels: %w", err)
	}
	
	log.Println("Reconciliation cycle completed successfully")
	return nil
}

// ConfigureReplication configures master/replica relationships in the cluster
func (c *MemgraphController) ConfigureReplication(ctx context.Context, clusterState *ClusterState) error {
	if len(clusterState.Pods) == 0 {
		log.Println("No pods to configure replication for")
		return nil
	}

	log.Println("Starting replication configuration...")
	currentMaster := clusterState.CurrentMaster
	
	if currentMaster == "" {
		return fmt.Errorf("no master pod selected for replication configuration")
	}

	log.Printf("Configuring replication with master: %s", currentMaster)

	// Phase 1: Configure pod roles (MAIN/REPLICA)
	var configErrors []error
	
	for podName, podInfo := range clusterState.Pods {
		if !podInfo.NeedsReplicationConfiguration(currentMaster) {
			log.Printf("Pod %s already in correct replication state", podName)
			continue
		}

		if podInfo.ShouldBecomeMaster(currentMaster) {
			log.Printf("Promoting pod %s to MASTER role", podName)
			if err := c.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, podInfo.BoltAddress); err != nil {
				log.Printf("Failed to promote pod %s to MASTER: %v", podName, err)
				configErrors = append(configErrors, fmt.Errorf("promote %s to MASTER: %w", podName, err))
				continue
			}
			log.Printf("Successfully promoted pod %s to MASTER", podName)
		}

		if podInfo.ShouldBecomeReplica(currentMaster) {
			log.Printf("Demoting pod %s to REPLICA role", podName)
			if err := c.memgraphClient.SetReplicationRoleToReplicaWithRetry(ctx, podInfo.BoltAddress); err != nil {
				log.Printf("Failed to demote pod %s to REPLICA: %v", podName, err)
				configErrors = append(configErrors, fmt.Errorf("demote %s to REPLICA: %w", podName, err))
				continue
			}
			log.Printf("Successfully demoted pod %s to REPLICA", podName)
		}
	}

	// Phase 2: Configure SYNC/ASYNC replication strategy with controller state authority
	if err := c.configureReplicationWithEnhancedSyncStrategy(ctx, clusterState); err != nil {
		log.Printf("Failed to configure enhanced SYNC/ASYNC replication: %v", err)
		configErrors = append(configErrors, fmt.Errorf("enhanced SYNC/ASYNC replication configuration: %w", err))
	}

	// Phase 2.5: Monitor SYNC replica health and handle failures
	if err := c.detectSyncReplicaHealth(ctx, clusterState); err != nil {
		log.Printf("SYNC replica health check failed: %v", err)
		configErrors = append(configErrors, fmt.Errorf("SYNC replica health monitoring: %w", err))
	}

	// Phase 3: Handle any existing replicas that should be removed
	// TEMPORARILY DISABLED: Conservative cleanup to prevent state corruption
	log.Printf("Skipping replica cleanup phase to preserve existing replication state")
	// if err := c.cleanupObsoleteReplicas(ctx, clusterState); err != nil {
	//	log.Printf("Warning: failed to cleanup obsolete replicas: %v", err)
	//	configErrors = append(configErrors, fmt.Errorf("cleanup obsolete replicas: %w", err))
	// }

	if len(configErrors) > 0 {
		log.Printf("Replication configuration completed with %d errors:", len(configErrors))
		for _, err := range configErrors {
			log.Printf("  - %v", err)
		}
		return fmt.Errorf("replication configuration had %d errors (see logs for details)", len(configErrors))
	}

	log.Printf("Replication configuration completed successfully for %d pods", len(clusterState.Pods))
	return nil
}

// selectSyncReplica determines which replica should be configured as SYNC using controller state authority
func (c *MemgraphController) selectSyncReplica(clusterState *ClusterState, currentMaster string) string {
	log.Printf("Selecting SYNC replica using deterministic controller strategy...")
	
	// Strategy: Use controller's two-pod master/SYNC strategy
	// If pod-0 is master, pod-1 is SYNC replica (and vice versa)
	currentMasterIndex := c.config.ExtractPodIndex(currentMaster)
	
	if currentMasterIndex == 0 {
		// Master is pod-0, SYNC replica should be pod-1
		syncReplicaName := c.config.GetPodName(1)
		if _, exists := clusterState.Pods[syncReplicaName]; exists {
			log.Printf("Deterministic SYNC selection: %s (master=pod-0)", syncReplicaName)
			return syncReplicaName
		}
	} else if currentMasterIndex == 1 {
		// Master is pod-1, SYNC replica should be pod-0
		syncReplicaName := c.config.GetPodName(0)
		if _, exists := clusterState.Pods[syncReplicaName]; exists {
			log.Printf("Deterministic SYNC selection: %s (master=pod-1)", syncReplicaName)
			return syncReplicaName
		}
	}
	
	// Fallback: master is not pod-0 or pod-1, or target SYNC replica doesn't exist
	// Use alphabetical selection as fallback
	log.Printf("Fallback SYNC selection: master index=%d not in eligible range", currentMasterIndex)
	return c.selectSyncReplicaFallback(clusterState, currentMaster)
}

// selectSyncReplicaFallback uses alphabetical selection as fallback
func (c *MemgraphController) selectSyncReplicaFallback(clusterState *ClusterState, currentMaster string) string {
	var eligibleReplicas []string
	
	// Collect all non-master pods as potential SYNC replicas
	for podName := range clusterState.Pods {
		if podName != currentMaster {
			eligibleReplicas = append(eligibleReplicas, podName)
		}
	}
	
	if len(eligibleReplicas) == 0 {
		return "" // No replicas available
	}
	
	// Sort alphabetically to ensure deterministic selection
	for i := 0; i < len(eligibleReplicas)-1; i++ {
		for j := i + 1; j < len(eligibleReplicas); j++ {
			if eligibleReplicas[i] > eligibleReplicas[j] {
				eligibleReplicas[i], eligibleReplicas[j] = eligibleReplicas[j], eligibleReplicas[i]
			}
		}
	}
	
	log.Printf("Fallback SYNC replica selected: %s (alphabetical first)", eligibleReplicas[0])
	return eligibleReplicas[0]
}

// identifySyncReplica finds which replica (if any) is currently configured as SYNC
func (c *MemgraphController) identifySyncReplica(clusterState *ClusterState, currentMaster string) string {
	masterPod, exists := clusterState.Pods[currentMaster]
	if !exists {
		return ""
	}
	
	// Check ReplicasInfo for any SYNC replica
	for _, replica := range masterPod.ReplicasInfo {
		if replica.SyncMode == "sync" { // Memgraph returns lowercase "sync"
			// Convert replica name back to pod name (underscores to dashes)
			podName := strings.ReplaceAll(replica.Name, "_", "-")
			return podName
		}
	}
	
	return "" // No SYNC replica found
}

// configureReplicationWithEnhancedSyncStrategy configures replication using enhanced SYNC/ASYNC strategy with controller authority
func (c *MemgraphController) configureReplicationWithEnhancedSyncStrategy(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Configuring enhanced SYNC/ASYNC replication with controller state authority...")
	
	currentMaster := clusterState.CurrentMaster
	if currentMaster == "" {
		return fmt.Errorf("no master pod selected for enhanced SYNC replication configuration")
	}
	
	_, exists := clusterState.Pods[currentMaster]
	if !exists {
		return fmt.Errorf("master pod %s not found in cluster state", currentMaster)
	}
	
	// Use controller state authority to determine SYNC replica
	targetSyncReplica := c.selectSyncReplica(clusterState, currentMaster)
	currentSyncReplica := c.identifySyncReplica(clusterState, currentMaster)
	
	log.Printf("Enhanced SYNC strategy: target=%s, current=%s, master=%s", 
		targetSyncReplica, currentSyncReplica, currentMaster)
	
	var configErrors []error
	
	// Phase 1: Ensure SYNC replica is configured correctly
	if err := c.ensureCorrectSyncReplica(ctx, clusterState, targetSyncReplica, currentSyncReplica); err != nil {
		configErrors = append(configErrors, fmt.Errorf("SYNC replica configuration: %w", err))
	}
	
	// Phase 2: Configure remaining pods as ASYNC replicas
	if err := c.configureAsyncReplicas(ctx, clusterState, targetSyncReplica); err != nil {
		configErrors = append(configErrors, fmt.Errorf("ASYNC replica configuration: %w", err))
	}
	
	// Phase 3: Verify final SYNC replica configuration
	if err := c.verifySyncReplicaConfiguration(ctx, clusterState); err != nil {
		log.Printf("⚠️  SYNC replica verification failed: %v", err)
		configErrors = append(configErrors, fmt.Errorf("SYNC replica verification: %w", err))
	}
	
	if len(configErrors) > 0 {
		log.Printf("Enhanced SYNC replication configuration completed with %d errors:", len(configErrors))
		for _, err := range configErrors {
			log.Printf("  - %v", err)
		}
		// Continue despite errors - partial configuration is better than none
	}
	
	log.Printf("✅ Enhanced SYNC replication configuration completed")
	return nil
}

// ensureCorrectSyncReplica ensures the correct pod is configured as SYNC replica
func (c *MemgraphController) ensureCorrectSyncReplica(ctx context.Context, clusterState *ClusterState, targetSyncReplica, currentSyncReplica string) error {
	if targetSyncReplica == "" {
		log.Printf("No target SYNC replica available - skipping SYNC configuration")
		return nil
	}
	
	// If target matches current, verify it's correctly configured
	if targetSyncReplica == currentSyncReplica {
		log.Printf("SYNC replica %s already correctly assigned", targetSyncReplica)
		return c.verifyPodSyncConfiguration(ctx, clusterState, targetSyncReplica)
	}
	
	// Need to change SYNC replica assignment
	log.Printf("Changing SYNC replica: %s → %s", currentSyncReplica, targetSyncReplica)
	
	// Step 1: Remove old SYNC replica if it exists
	if currentSyncReplica != "" {
		if err := c.demoteSyncToAsync(ctx, clusterState, currentSyncReplica); err != nil {
			log.Printf("Warning: Failed to demote old SYNC replica %s: %v", currentSyncReplica, err)
		}
	}
	
	// Step 2: Configure new SYNC replica
	if err := c.configurePodAsSyncReplica(ctx, clusterState, targetSyncReplica); err != nil {
		return fmt.Errorf("failed to configure new SYNC replica %s: %w", targetSyncReplica, err)
	}
	
	log.Printf("✅ SYNC replica successfully changed to %s", targetSyncReplica)
	return nil
}

// configureAsyncReplicas configures all non-master, non-SYNC pods as ASYNC replicas
func (c *MemgraphController) configureAsyncReplicas(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) error {
	currentMaster := clusterState.CurrentMaster
	masterPod := clusterState.Pods[currentMaster]
	var configErrors []error
	
	for podName, podInfo := range clusterState.Pods {
		// Skip master and SYNC replica
		if podName == currentMaster || podName == syncReplicaPod {
			continue
		}
		
		// Configure as ASYNC replica
		if err := c.configurePodAsAsyncReplica(ctx, masterPod, podInfo); err != nil {
			log.Printf("Warning: Failed to configure ASYNC replica %s: %v", podName, err)
			configErrors = append(configErrors, fmt.Errorf("ASYNC replica %s: %w", podName, err))
			continue
		}
		
		log.Printf("✅ Configured %s as ASYNC replica", podName)
	}
	
	if len(configErrors) > 0 {
		return fmt.Errorf("ASYNC replica configuration had %d errors", len(configErrors))
	}
	
	return nil
}

// configurePodAsAsyncReplica configures a specific pod as ASYNC replica
func (c *MemgraphController) configurePodAsAsyncReplica(ctx context.Context, masterPod, replicaPod *PodInfo) error {
	replicaName := replicaPod.GetReplicaName()
	replicaAddress := replicaPod.GetReplicationAddress(c.config.ServiceName)
	
	// Check if already configured correctly as ASYNC
	for _, replica := range masterPod.ReplicasInfo {
		if replica.Name == replicaName && replica.SyncMode == "async" {
			replicaPod.IsSyncReplica = false
			return nil // Already configured correctly
		}
	}
	
	// Need to register/re-register as ASYNC
	// Drop existing registration first (if any)
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replicaName); err != nil {
		log.Printf("Warning: Could not drop existing replica %s: %v", replicaName, err)
	}
	
	// Register as ASYNC replica
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress, "ASYNC"); err != nil {
		return fmt.Errorf("failed to register ASYNC replica %s: %w", replicaName, err)
	}
	
	// Update tracking
	replicaPod.IsSyncReplica = false
	return nil
}

// demoteSyncToAsync demotes a SYNC replica to ASYNC mode
func (c *MemgraphController) demoteSyncToAsync(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) error {
	log.Printf("Demoting SYNC replica %s to ASYNC mode", syncReplicaPod)
	
	syncPod, exists := clusterState.Pods[syncReplicaPod]
	if !exists {
		return fmt.Errorf("SYNC replica pod %s not found", syncReplicaPod)
	}
	
	masterPod := clusterState.Pods[clusterState.CurrentMaster]
	replicaName := syncPod.GetReplicaName()
	replicaAddress := syncPod.GetReplicationAddress(c.config.ServiceName)
	
	// Drop SYNC registration
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replicaName); err != nil {
		return fmt.Errorf("failed to drop SYNC replica %s: %w", replicaName, err)
	}
	
	// Re-register as ASYNC
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress, "ASYNC"); err != nil {
		return fmt.Errorf("failed to re-register %s as ASYNC: %w", replicaName, err)
	}
	
	// Update tracking
	syncPod.IsSyncReplica = false
	log.Printf("Successfully demoted %s from SYNC to ASYNC", syncReplicaPod)
	
	return nil
}

// verifyPodSyncConfiguration verifies that a pod is correctly configured as SYNC replica
func (c *MemgraphController) verifyPodSyncConfiguration(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) error {
	syncPod := clusterState.Pods[syncReplicaPod]
	masterPod := clusterState.Pods[clusterState.CurrentMaster]
	replicaName := syncPod.GetReplicaName()
	
	// Check if registered as SYNC with master
	for _, replica := range masterPod.ReplicasInfo {
		if replica.Name == replicaName {
			if replica.SyncMode == "sync" {
				syncPod.IsSyncReplica = true
				return nil
			}
			return fmt.Errorf("pod %s registered with sync_mode=%s, expected 'sync'", syncReplicaPod, replica.SyncMode)
		}
	}
	
	return fmt.Errorf("pod %s not registered as replica with master", syncReplicaPod)
}

// verifySyncReplicaConfiguration verifies the final SYNC replica configuration
func (c *MemgraphController) verifySyncReplicaConfiguration(ctx context.Context, clusterState *ClusterState) error {
	// Count SYNC replicas
	syncReplicaCount := 0
	var syncReplicaName string
	
	for podName, podInfo := range clusterState.Pods {
		if podInfo.IsSyncReplica {
			syncReplicaCount++
			syncReplicaName = podName
		}
	}
	
	// Validate SYNC replica count
	switch syncReplicaCount {
	case 0:
		log.Printf("⚠️  WARNING: No SYNC replica configured - cluster may have write availability issues")
		return fmt.Errorf("no SYNC replica configured")
		
	case 1:
		log.Printf("✅ SYNC replica verification passed: %s", syncReplicaName)
		return nil
		
	default:
		log.Printf("❌ ERROR: Multiple SYNC replicas detected (%d) - this should not happen", syncReplicaCount)
		return fmt.Errorf("multiple SYNC replicas detected: %d", syncReplicaCount)
	}
}

// configureReplicationWithSyncStrategy configures replication using SYNC/ASYNC strategy (legacy method)
func (c *MemgraphController) configureReplicationWithSyncStrategy(ctx context.Context, clusterState *ClusterState) error {
	currentMaster := clusterState.CurrentMaster
	if currentMaster == "" {
		return fmt.Errorf("no master pod selected for SYNC replication configuration")
	}
	
	masterPod, exists := clusterState.Pods[currentMaster]
	if !exists {
		return fmt.Errorf("master pod %s not found in cluster state", currentMaster)
	}
	
	log.Printf("Configuring SYNC/ASYNC replication with master: %s", currentMaster)
	
	// Identify or select SYNC replica
	currentSyncReplica := c.identifySyncReplica(clusterState, currentMaster)
	targetSyncReplica := c.selectSyncReplica(clusterState, currentMaster)
	
	if targetSyncReplica == "" {
		log.Println("No replicas available for SYNC configuration")
		return nil
	}
	
	log.Printf("Target SYNC replica: %s (current: %s)", targetSyncReplica, currentSyncReplica)
	
	var configErrors []error
	
	// Phase 1: Configure SYNC replica (highest priority - must succeed)
	if targetSyncReplica != currentSyncReplica {
		log.Printf("Configuring %s as SYNC replica", targetSyncReplica)
		
		targetPod, exists := clusterState.Pods[targetSyncReplica]
		if !exists {
			return fmt.Errorf("target SYNC replica pod %s not found", targetSyncReplica)
		}
		
		replicaName := targetPod.GetReplicaName()
		replicaAddress := targetPod.GetReplicationAddress(c.config.ServiceName)
		
		// Check if already configured as SYNC
		isAlreadySyncReplica := false
		currentMode := ""
		for _, replica := range masterPod.ReplicasInfo {
			if replica.Name == replicaName {
				currentMode = replica.SyncMode
				if currentMode == "sync" {
					isAlreadySyncReplica = true
				}
				break
			}
		}
		
		if isAlreadySyncReplica {
			log.Printf("SYNC replica %s already correctly configured, skipping", replicaName)
			targetPod.IsSyncReplica = true
		} else {
			// Only drop if changing mode (ASYNC -> SYNC) or not registered
			if currentMode != "" && currentMode != "sync" {
				log.Printf("Changing replica %s from %s to SYNC mode", replicaName, currentMode)
				if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replicaName); err != nil {
					log.Printf("Warning: Could not drop existing replica %s: %v", replicaName, err)
				}
			}
			
			// Register as SYNC replica (CRITICAL - must succeed)
			if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
				configErrors = append(configErrors, fmt.Errorf("CRITICAL: failed to register SYNC replica %s: %w", replicaName, err))
				return fmt.Errorf("CRITICAL: Cannot guarantee data consistency without SYNC replica: %w", err)
			}
			
			log.Printf("Successfully registered SYNC replica: %s", replicaName)
			
			// Update pod tracking
			targetPod.IsSyncReplica = true
		}
		
		// Clear old SYNC replica flag if different
		if currentSyncReplica != "" && currentSyncReplica != targetSyncReplica {
			if oldSyncPod, exists := clusterState.Pods[currentSyncReplica]; exists {
				oldSyncPod.IsSyncReplica = false
			}
		}
	}
	
	// Phase 2: Configure ASYNC replicas (failures are warnings, not critical)
	for podName, podInfo := range clusterState.Pods {
		// Skip master pod and SYNC replica
		if podName == currentMaster || podName == targetSyncReplica {
			continue
		}
		
		replicaName := podInfo.GetReplicaName()
		replicaAddress := podInfo.GetReplicationAddress(c.config.ServiceName)
		
		// Check if replica is already registered with correct mode
		isAlreadyConfigured := false
		currentMode := ""
		for _, replica := range masterPod.ReplicasInfo {
			if replica.Name == replicaName {
				isAlreadyConfigured = true
				currentMode = replica.SyncMode
				break
			}
		}
		
		if isAlreadyConfigured && currentMode == "async" {
			log.Printf("ASYNC replica %s already correctly configured, skipping", replicaName)
			podInfo.IsSyncReplica = false
			continue
		}
		
		log.Printf("Configuring %s as ASYNC replica", replicaName)
		
		// Only drop if mode needs to change (SYNC -> ASYNC) or not registered
		if isAlreadyConfigured && currentMode != "async" {
			log.Printf("Changing replica %s from %s to ASYNC mode", replicaName, currentMode)
			if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replicaName); err != nil {
				log.Printf("Warning: Could not drop existing replica %s: %v", replicaName, err)
			}
		}
		
		// Register as ASYNC replica (only if needed)
		if !isAlreadyConfigured || currentMode != "async" {
			if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress, "ASYNC"); err != nil {
				log.Printf("Warning: failed to register ASYNC replica %s: %v", replicaName, err)
				configErrors = append(configErrors, fmt.Errorf("register ASYNC replica %s: %w", replicaName, err))
				continue
			}
			log.Printf("Successfully registered ASYNC replica: %s", replicaName)
		}
		
		podInfo.IsSyncReplica = false
	}
	
	if len(configErrors) > 0 {
		log.Printf("SYNC replication configuration completed with %d warnings:", len(configErrors))
		for _, err := range configErrors {
			log.Printf("  - %v", err)
		}
		// Don't return error for ASYNC replica failures - SYNC replica success is what matters
	}
	
	log.Printf("SYNC replication configuration completed: SYNC=%s, ASYNC replicas=%d", 
		targetSyncReplica, len(clusterState.Pods)-2) // -2 for master and SYNC replica
	
	return nil
}

// promoteAsyncToSync promotes an ASYNC replica to SYNC mode with comprehensive validation
func (c *MemgraphController) promoteAsyncToSync(ctx context.Context, clusterState *ClusterState, targetReplicaPod string) error {
	currentMaster := clusterState.CurrentMaster
	if currentMaster == "" {
		return fmt.Errorf("no master pod available for ASYNC→SYNC promotion")
	}
	
	masterPod, exists := clusterState.Pods[currentMaster]
	if !exists {
		return fmt.Errorf("master pod %s not found", currentMaster)
	}
	
	targetPod, exists := clusterState.Pods[targetReplicaPod]
	if !exists {
		return fmt.Errorf("target replica pod %s not found", targetReplicaPod)
	}
	
	log.Printf("🔄 ASYNC→SYNC PROMOTION: %s", targetReplicaPod)
	
	// Pre-promotion validation
	if err := c.validateAsyncToSyncPromotion(ctx, clusterState, targetReplicaPod); err != nil {
		return fmt.Errorf("ASYNC→SYNC promotion validation failed: %w", err)
	}
	
	replicaName := targetPod.GetReplicaName()
	replicaAddress := targetPod.GetReplicationAddress(c.config.ServiceName)
	
	// Step 1: Check if replica is caught up
	log.Printf("Checking if %s is caught up with master...", replicaName)
	if err := c.verifyReplicaSyncStatus(ctx, masterPod, replicaName); err != nil {
		log.Printf("⚠️  WARNING: Replica may not be fully caught up: %v", err)
		log.Printf("⚠️  Proceeding with promotion - verify data consistency manually")
	}
	
	// Step 2: Drop existing ASYNC registration
	log.Printf("Dropping existing ASYNC replica registration: %s", replicaName)
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replicaName); err != nil {
		return fmt.Errorf("failed to drop ASYNC replica %s: %w", replicaName, err)
	}
	
	// Step 3: Re-register as SYNC replica
	log.Printf("Re-registering %s as SYNC replica", replicaName)
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("failed to register SYNC replica %s: %w", replicaName, err)
	}
	
	// Step 4: Update tracking and clear old SYNC replica
	c.updateSyncReplicaTracking(clusterState, targetReplicaPod)
	
	// Step 5: Verify promotion success
	if err := c.verifyAsyncToSyncPromotion(ctx, clusterState, targetReplicaPod); err != nil {
		log.Printf("❌ SYNC promotion verification failed: %v", err)
		return fmt.Errorf("ASYNC→SYNC promotion verification failed: %w", err)
	}
	
	log.Printf("✅ Successfully promoted %s to SYNC replica", targetReplicaPod)
	log.Printf("📊 PROMOTION EVENT: %s promoted from ASYNC to SYNC", targetReplicaPod)
	
	return nil
}

// validateAsyncToSyncPromotion validates preconditions for ASYNC→SYNC promotion
func (c *MemgraphController) validateAsyncToSyncPromotion(ctx context.Context, clusterState *ClusterState, targetReplica string) error {
	// Check 1: Target replica must exist and be healthy
	targetPod, exists := clusterState.Pods[targetReplica]
	if !exists {
		return fmt.Errorf("target replica %s not found", targetReplica)
	}
	
	if !c.isPodHealthyForMaster(targetPod) {
		return fmt.Errorf("target replica %s is not healthy", targetReplica)
	}
	
	// Check 2: Target replica must have replica role
	if targetPod.MemgraphRole != "replica" {
		return fmt.Errorf("target pod %s has role %s, expected replica", targetReplica, targetPod.MemgraphRole)
	}
	
	// Check 3: Target replica must not already be SYNC
	if targetPod.IsSyncReplica {
		return fmt.Errorf("target replica %s is already a SYNC replica", targetReplica)
	}
	
	// Check 4: Master must be available
	masterPod, exists := clusterState.Pods[clusterState.CurrentMaster]
	if !exists || !c.isPodHealthyForMaster(masterPod) {
		return fmt.Errorf("master is not available for ASYNC→SYNC promotion")
	}
	
	log.Printf("✅ ASYNC→SYNC promotion validation passed for %s", targetReplica)
	return nil
}

// verifyReplicaSyncStatus checks if replica is caught up with master
func (c *MemgraphController) verifyReplicaSyncStatus(ctx context.Context, masterPod *PodInfo, replicaName string) error {
	// Query current replica status from master
	for _, replica := range masterPod.ReplicasInfo {
		if replica.Name == replicaName {
			// In Memgraph, system_timestamp represents sync progress
			// A replica is "caught up" when it's ready and not significantly behind
			if replica.SystemTimestamp >= 0 {
				log.Printf("Replica %s sync status: timestamp=%d", replicaName, replica.SystemTimestamp)
				return nil // Consider it caught up
			}
			return fmt.Errorf("replica %s appears to be behind (timestamp: %d)", replicaName, replica.SystemTimestamp)
		}
	}
	
	return fmt.Errorf("replica %s not found in master's replica list", replicaName)
}

// updateSyncReplicaTracking updates SYNC replica tracking after promotion
func (c *MemgraphController) updateSyncReplicaTracking(clusterState *ClusterState, newSyncReplica string) {
	// Clear old SYNC replica flags
	for _, podInfo := range clusterState.Pods {
		podInfo.IsSyncReplica = false
	}
	
	// Set new SYNC replica flag
	if newSyncPod, exists := clusterState.Pods[newSyncReplica]; exists {
		newSyncPod.IsSyncReplica = true
		log.Printf("Updated SYNC replica tracking: %s", newSyncReplica)
	}
}

// verifyAsyncToSyncPromotion verifies that ASYNC→SYNC promotion was successful
func (c *MemgraphController) verifyAsyncToSyncPromotion(ctx context.Context, clusterState *ClusterState, targetReplica string) error {
	// Re-query master's replica information to verify promotion
	masterPod := clusterState.Pods[clusterState.CurrentMaster]
	replicaName := clusterState.Pods[targetReplica].GetReplicaName()
	
	// Query fresh replica information
	replicasResp, err := c.memgraphClient.QueryReplicasWithRetry(ctx, masterPod.BoltAddress)
	if err != nil {
		return fmt.Errorf("failed to query replicas for verification: %w", err)
	}
	
	// Check if target replica is now registered as SYNC
	for _, replica := range replicasResp.Replicas {
		if replica.Name == replicaName {
			if replica.SyncMode == "sync" {
				log.Printf("✅ Verified: %s is now registered as SYNC replica", targetReplica)
				return nil
			}
			return fmt.Errorf("replica %s has sync_mode=%s, expected 'sync'", targetReplica, replica.SyncMode)
		}
	}
	
	return fmt.Errorf("replica %s not found in master's replica list after promotion", targetReplica)
}

// emergencyAsyncToSyncPromotion handles emergency ASYNC→SYNC promotion scenarios
func (c *MemgraphController) emergencyAsyncToSyncPromotion(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("🚨 EMERGENCY: SYNC replica unavailable - evaluating ASYNC→SYNC promotion")
	
	// Find the best ASYNC replica for emergency promotion
	bestAsyncReplica := c.selectBestAsyncReplicaForSyncPromotion(clusterState)
	if bestAsyncReplica == "" {
		return fmt.Errorf("no suitable ASYNC replica available for emergency SYNC promotion")
	}
	
	log.Printf("🔄 Emergency ASYNC→SYNC promotion candidate: %s", bestAsyncReplica)
	log.Printf("⚠️  WARNING: This may result in data inconsistency if replica is not caught up")
	
	// Perform emergency promotion
	if err := c.promoteAsyncToSync(ctx, clusterState, bestAsyncReplica); err != nil {
		return fmt.Errorf("emergency ASYNC→SYNC promotion failed: %w", err)
	}
	
	log.Printf("📊 EMERGENCY PROMOTION EVENT: %s promoted to SYNC due to SYNC replica failure", bestAsyncReplica)
	return nil
}

// selectBestAsyncReplicaForSyncPromotion selects the best ASYNC replica for SYNC promotion
func (c *MemgraphController) selectBestAsyncReplicaForSyncPromotion(clusterState *ClusterState) string {
	currentMaster := clusterState.CurrentMaster
	targetSyncReplicaName := c.selectSyncReplica(clusterState, currentMaster)
	
	// First preference: the pod that should normally be SYNC replica
	if targetPod, exists := clusterState.Pods[targetSyncReplicaName]; exists {
		if c.isPodHealthyForMaster(targetPod) && targetPod.MemgraphRole == "replica" && !targetPod.IsSyncReplica {
			log.Printf("Best ASYNC→SYNC candidate: %s (normal SYNC replica pod)", targetSyncReplicaName)
			return targetSyncReplicaName
		}
	}
	
	// Second preference: healthy ASYNC replicas, lowest index first
	var candidates []string
	for podName, podInfo := range clusterState.Pods {
		if podName != currentMaster && 
		   !podInfo.IsSyncReplica && 
		   podInfo.MemgraphRole == "replica" && 
		   c.isPodHealthyForMaster(podInfo) {
			candidates = append(candidates, podName)
		}
	}
	
	if len(candidates) == 0 {
		return ""
	}
	
	// Sort by index (deterministic selection)
	var bestCandidate string
	bestIndex := 999
	for _, candidate := range candidates {
		index := c.config.ExtractPodIndex(candidate)
		if index >= 0 && index < bestIndex {
			bestIndex = index
			bestCandidate = candidate
		}
	}
	
	if bestCandidate != "" {
		log.Printf("Best ASYNC→SYNC candidate: %s (lowest index %d)", bestCandidate, bestIndex)
	}
	
	return bestCandidate
}

// handleSyncReplicaFailure responds to SYNC replica becoming unavailable with enhanced emergency procedures
func (c *MemgraphController) handleSyncReplicaFailure(ctx context.Context, clusterState *ClusterState, failedSyncReplica string) error {
	currentMaster := clusterState.CurrentMaster
	if currentMaster == "" {
		return fmt.Errorf("no master available during SYNC replica failure")
	}
	
	log.Printf("🚨 CRITICAL: SYNC replica %s is down - master will block all writes", failedSyncReplica)
	log.Printf("Write operations will be blocked until SYNC replica is restored")
	
	// Check for emergency ASYNC→SYNC promotion options
	bestCandidate := c.selectBestAsyncReplicaForSyncPromotion(clusterState)
	
	if bestCandidate == "" {
		log.Printf("❌ CRITICAL: No healthy ASYNC replicas available for emergency promotion")
		log.Printf("🔧 Manual intervention required:")
		log.Printf("  1. Restart failed SYNC replica pod: kubectl delete pod %s", failedSyncReplica)
		log.Printf("  2. Check pod health: kubectl get pods -n %s", c.config.Namespace)
		log.Printf("  3. Check master connectivity to replicas")
		return fmt.Errorf("no healthy ASYNC replicas for emergency SYNC promotion")
	}
	
	log.Printf("🔧 Recovery options available:")
	log.Printf("  1. 🔄 AUTOMATIC: Emergency ASYNC→SYNC promotion of %s", bestCandidate)
	log.Printf("  2. 🚀 RESTART: Restart failed SYNC replica pod")
	log.Printf("  3. 🔧 MANUAL: Manual intervention and custom recovery")
	
	// Evaluate if automatic emergency promotion should be performed
	if c.shouldPerformEmergencyPromotion(clusterState) {
		log.Printf("🔄 Performing automatic emergency ASYNC→SYNC promotion...")
		
		if err := c.emergencyAsyncToSyncPromotion(ctx, clusterState); err != nil {
			log.Printf("❌ Emergency promotion failed: %v", err)
			log.Printf("🔧 Manual intervention required")
			return c.provideManaualRecoveryInstructions(clusterState, failedSyncReplica, bestCandidate)
		}
		
		log.Printf("✅ Emergency ASYNC→SYNC promotion completed successfully")
		log.Printf("⚠️  WARNING: Verify data consistency after emergency promotion")
		return nil
	}
	
	// Provide manual recovery instructions
	return c.provideManaualRecoveryInstructions(clusterState, failedSyncReplica, bestCandidate)
}

// shouldPerformEmergencyPromotion determines if automatic emergency promotion should be performed
func (c *MemgraphController) shouldPerformEmergencyPromotion(clusterState *ClusterState) bool {
	// Conservative approach: only perform automatic promotion if:
	// 1. We have a suitable candidate
	// 2. The candidate is the expected SYNC replica pod (pod-0 or pod-1)
	// 3. The cluster is in a healthy state otherwise
	
	bestCandidate := c.selectBestAsyncReplicaForSyncPromotion(clusterState)
	if bestCandidate == "" {
		return false
	}
	
	// Only auto-promote if candidate is pod-0 or pod-1 (expected SYNC replica range)
	candidateIndex := c.config.ExtractPodIndex(bestCandidate)
	if candidateIndex < 0 || candidateIndex > 1 {
		log.Printf("Emergency candidate %s is not in expected SYNC range (index %d) - requiring manual intervention", 
			bestCandidate, candidateIndex)
		return false
	}
	
	// Check cluster health
	healthSummary := clusterState.GetClusterHealthSummary()
	healthyPods := healthSummary["healthy_pods"].(int)
	totalPods := healthSummary["total_pods"].(int)
	
	if healthyPods < totalPods/2 {
		log.Printf("Cluster health insufficient for automatic promotion: %d/%d pods healthy", healthyPods, totalPods)
		return false
	}
	
	log.Printf("✅ Automatic emergency promotion criteria met for %s", bestCandidate)
	return true
}

// provideManaualRecoveryInstructions provides detailed manual recovery instructions
func (c *MemgraphController) provideManaualRecoveryInstructions(clusterState *ClusterState, failedSyncReplica, bestCandidate string) error {
	log.Printf("🔧 Manual SYNC replica recovery instructions:")
	log.Printf("")
	log.Printf("Option 1: Restart failed SYNC replica (preferred)")
	log.Printf("  kubectl delete pod %s -n %s", failedSyncReplica, c.config.Namespace)
	log.Printf("  # Wait for pod to restart and reconnect")
	log.Printf("")
	log.Printf("Option 2: Emergency ASYNC→SYNC promotion")
	if bestCandidate != "" {
		replicaName := strings.ReplaceAll(bestCandidate, "-", "_")
		log.Printf("  # Drop failed SYNC replica:")
		log.Printf("  kubectl exec %s -n %s -- mgconsole -e \"DROP REPLICA %s;\"", 
			clusterState.CurrentMaster, c.config.Namespace, strings.ReplaceAll(failedSyncReplica, "-", "_"))
		log.Printf("  # Promote ASYNC replica to SYNC:")
		log.Printf("  kubectl exec %s -n %s -- mgconsole -e \"DROP REPLICA %s; REGISTER REPLICA %s SYNC TO \\\"%s.%s:10000\\\";\"",
			clusterState.CurrentMaster, c.config.Namespace, replicaName, replicaName, bestCandidate, c.config.ServiceName)
	}
	log.Printf("")
	log.Printf("Option 3: Drop SYNC requirement (data loss risk)")
	log.Printf("  kubectl exec %s -n %s -- mgconsole -e \"DROP REPLICA %s;\"", 
		clusterState.CurrentMaster, c.config.Namespace, strings.ReplaceAll(failedSyncReplica, "-", "_"))
	log.Printf("  # WARNING: Writes will resume but data consistency not guaranteed")
	log.Printf("")
	log.Printf("⚠️  After manual intervention, restart the controller to resync state")
	
	return fmt.Errorf("SYNC replica failure requires manual intervention - see recovery instructions above")
}

// detectSyncReplicaHealth monitors SYNC replica availability and triggers emergency procedures
func (c *MemgraphController) detectSyncReplicaHealth(ctx context.Context, clusterState *ClusterState) error {
	currentMaster := clusterState.CurrentMaster
	if currentMaster == "" {
		return nil // No master, nothing to check
	}
	
	// Find current SYNC replica
	var syncReplicaPod string
	for podName, podInfo := range clusterState.Pods {
		if podInfo.IsSyncReplica {
			syncReplicaPod = podName
			break
		}
	}
	
	if syncReplicaPod == "" {
		log.Printf("⚠️  No SYNC replica configured - cluster may have write availability issues")
		
		// Try to configure SYNC replica if one should exist
		return c.ensureSyncReplicaExists(ctx, clusterState)
	}
	
	// Comprehensive SYNC replica health check
	healthStatus := c.checkSyncReplicaHealth(ctx, clusterState, syncReplicaPod)
	
	switch healthStatus {
	case SyncReplicaHealthy:
		log.Printf("✅ SYNC replica %s is healthy", syncReplicaPod)
		return nil
		
	case SyncReplicaUnhealthy:
		log.Printf("⚠️  SYNC replica %s is unhealthy - attempting recovery", syncReplicaPod)
		return c.recoverSyncReplica(ctx, clusterState, syncReplicaPod)
		
	case SyncReplicaFailed:
		log.Printf("❌ SYNC replica %s has failed - initiating emergency procedures", syncReplicaPod)
		return c.handleSyncReplicaFailure(ctx, clusterState, syncReplicaPod)
		
	default:
		log.Printf("Unknown SYNC replica health status for %s", syncReplicaPod)
		return nil
	}
}

// SyncReplicaHealthStatus represents the health status of a SYNC replica
type SyncReplicaHealthStatus int

const (
	SyncReplicaHealthy SyncReplicaHealthStatus = iota
	SyncReplicaUnhealthy
	SyncReplicaFailed
)

// checkSyncReplicaHealth performs comprehensive health check on SYNC replica
func (c *MemgraphController) checkSyncReplicaHealth(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) SyncReplicaHealthStatus {
	syncPod := clusterState.Pods[syncReplicaPod]
	
	// Check 1: Pod must have IP address
	if syncPod.BoltAddress == "" {
		log.Printf("SYNC replica health check failed: no bolt address")
		return SyncReplicaFailed
	}
	
	// Check 2: Pod must have Memgraph role information
	if syncPod.MemgraphRole == "" {
		log.Printf("SYNC replica health check failed: no Memgraph role info")
		return SyncReplicaUnhealthy
	}
	
	// Check 3: Pod must have replica role
	if syncPod.MemgraphRole != "replica" {
		log.Printf("SYNC replica health check failed: role is %s, expected replica", syncPod.MemgraphRole)
		return SyncReplicaFailed
	}
	
	// Check 4: Test actual connectivity
	if err := c.memgraphClient.TestConnectionWithRetry(ctx, syncPod.BoltAddress); err != nil {
		log.Printf("SYNC replica health check failed: connectivity error: %v", err)
		return SyncReplicaFailed
	}
	
	// Check 5: Verify it's actually registered as SYNC with master
	masterPod := clusterState.Pods[clusterState.CurrentMaster]
	if masterPod != nil {
		isRegisteredAsSync := false
		for _, replica := range masterPod.ReplicasInfo {
			if replica.Name == syncPod.GetReplicaName() && replica.SyncMode == "sync" {
				isRegisteredAsSync = true
				break
			}
		}
		
		if !isRegisteredAsSync {
			log.Printf("SYNC replica health check failed: not registered as SYNC with master")
			return SyncReplicaUnhealthy
		}
	}
	
	return SyncReplicaHealthy
}

// ensureSyncReplicaExists ensures a SYNC replica is configured when none exists
func (c *MemgraphController) ensureSyncReplicaExists(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Ensuring SYNC replica exists...")
	
	currentMaster := clusterState.CurrentMaster
	targetSyncReplica := c.selectSyncReplica(clusterState, currentMaster)
	
	if targetSyncReplica == "" {
		log.Printf("No suitable pods available for SYNC replica configuration")
		return nil
	}
	
	log.Printf("Configuring missing SYNC replica: %s", targetSyncReplica)
	
	// Configure the selected pod as SYNC replica
	return c.configurePodAsSyncReplica(ctx, clusterState, targetSyncReplica)
}

// recoverSyncReplica attempts to recover an unhealthy SYNC replica
func (c *MemgraphController) recoverSyncReplica(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) error {
	log.Printf("Attempting SYNC replica recovery for %s", syncReplicaPod)
	
	syncPod := clusterState.Pods[syncReplicaPod]
	masterPod := clusterState.Pods[clusterState.CurrentMaster]
	
	if masterPod == nil {
		return fmt.Errorf("no master pod available for SYNC replica recovery")
	}
	
	// Attempt to re-register as SYNC replica
	replicaName := syncPod.GetReplicaName()
	replicaAddress := syncPod.GetReplicationAddress(c.config.ServiceName)
	
	log.Printf("Re-registering %s as SYNC replica", replicaName)
	
	// Drop existing registration first
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replicaName); err != nil {
		log.Printf("Warning: Could not drop existing replica registration: %v", err)
	}
	
	// Re-register as SYNC
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("failed to re-register SYNC replica: %w", err)
	}
	
	log.Printf("Successfully recovered SYNC replica: %s", syncReplicaPod)
	return nil
}

// configurePodAsSyncReplica configures a specific pod as SYNC replica
func (c *MemgraphController) configurePodAsSyncReplica(ctx context.Context, clusterState *ClusterState, podName string) error {
	targetPod, exists := clusterState.Pods[podName]
	if !exists {
		return fmt.Errorf("target pod %s not found", podName)
	}
	
	masterPod, exists := clusterState.Pods[clusterState.CurrentMaster]
	if !exists {
		return fmt.Errorf("master pod %s not found", clusterState.CurrentMaster)
	}
	
	// Ensure target pod is in replica role
	if targetPod.MemgraphRole != "replica" {
		log.Printf("Setting %s to replica role before SYNC configuration", podName)
		if err := c.memgraphClient.SetReplicationRoleToReplicaWithRetry(ctx, targetPod.BoltAddress); err != nil {
			return fmt.Errorf("failed to set pod %s to replica role: %w", podName, err)
		}
	}
	
	// Register as SYNC replica
	replicaName := targetPod.GetReplicaName()
	replicaAddress := targetPod.GetReplicationAddress(c.config.ServiceName)
	
	log.Printf("Registering %s as SYNC replica", podName)
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("failed to register SYNC replica %s: %w", podName, err)
	}
	
	// Update tracking
	targetPod.IsSyncReplica = true
	log.Printf("Successfully configured %s as SYNC replica", podName)
	
	return nil
}

// cleanupObsoleteReplicas removes replica registrations that are no longer needed
// CONSERVATIVE: Only drops replicas that are definitively obsolete, not temporarily unreachable
func (c *MemgraphController) cleanupObsoleteReplicas(ctx context.Context, clusterState *ClusterState) error {
	currentMaster := clusterState.CurrentMaster
	if currentMaster == "" {
		return nil
	}

	masterPod, exists := clusterState.Pods[currentMaster]
	if !exists {
		return fmt.Errorf("master pod %s not found", currentMaster)
	}

	// Get current replicas from the master
	replicasResp, err := c.memgraphClient.QueryReplicasWithRetry(ctx, masterPod.BoltAddress)
	if err != nil {
		return fmt.Errorf("failed to query current replicas from master: %w", err)
	}

	// Build set of ALL known pod names (including temporarily unreachable ones)
	// We only want to drop replicas for pods that are definitively gone
	allKnownPods := make(map[string]bool)
	runningPods := make(map[string]bool)
	
	for podName, podInfo := range clusterState.Pods {
		if podName != currentMaster {
			replicaName := podInfo.GetReplicaName()
			allKnownPods[replicaName] = true
			// Only mark as running if we can actually query it
			if podInfo.MemgraphRole != "" {
				runningPods[replicaName] = true
			}
		}
	}

	// CONSERVATIVE CLEANUP: Only drop replicas that are definitely obsolete
	var cleanupErrors []error
	for _, replica := range replicasResp.Replicas {
		if !allKnownPods[replica.Name] {
			// This replica doesn't correspond to any known pod - safe to drop
			log.Printf("Dropping truly obsolete replica %s (no corresponding pod found)", replica.Name)
			if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replica.Name); err != nil {
				log.Printf("Failed to drop obsolete replica %s: %v", replica.Name, err)
				cleanupErrors = append(cleanupErrors, fmt.Errorf("drop replica %s: %w", replica.Name, err))
			} else {
				log.Printf("Successfully dropped obsolete replica %s", replica.Name)
			}
		} else if !runningPods[replica.Name] {
			// Pod exists but is temporarily unreachable - DO NOT DROP
			log.Printf("Keeping replica %s (pod temporarily unreachable, will recover)", replica.Name)
		} else {
			// Pod exists and is running - keep replica
			log.Printf("Keeping replica %s (pod healthy)", replica.Name)
		}
	}

	if len(cleanupErrors) > 0 {
		return fmt.Errorf("cleanup had %d errors: %v", len(cleanupErrors), cleanupErrors)
	}

	return nil
}

// SyncPodLabels synchronizes pod labels with their actual replication state
func (c *MemgraphController) SyncPodLabels(ctx context.Context, clusterState *ClusterState) error {
	if len(clusterState.Pods) == 0 {
		log.Println("No pods to sync labels for")
		return nil
	}

	log.Println("Starting pod label synchronization...")

	// Update cluster state with current pod states after replication configuration
	for podName, podInfo := range clusterState.Pods {
		// Reclassify state based on current information
		podInfo.State = podInfo.ClassifyState()
		log.Printf("Pod %s final state: %s (MemgraphRole=%s)", 
			podName, podInfo.State, podInfo.MemgraphRole)
	}

	log.Println("Replication configuration completed successfully")
	return nil
}

// GetClusterStatus collects current cluster status for API response
func (c *MemgraphController) GetClusterStatus(ctx context.Context) (*StatusResponse, error) {
	log.Println("Collecting cluster status for API...")
	
	// Discover current cluster state with all Memgraph queries
	clusterState, err := c.DiscoverCluster(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover cluster state: %w", err)
	}

	// Build cluster summary
	totalPods := len(clusterState.Pods)
	healthyPods := 0
	podStatuses := make([]PodStatus, 0, totalPods)
	
	// Find current SYNC replica
	var currentSyncReplica string
	var syncReplicaHealthy bool
	for podName, podInfo := range clusterState.Pods {
		if podInfo.IsSyncReplica {
			currentSyncReplica = podName
			// SYNC replica is healthy if we can query its Memgraph role
			syncReplicaHealthy = podInfo.MemgraphRole != ""
			break
		}
	}

	// Process each pod to determine health and build status
	for _, podInfo := range clusterState.Pods {
		// Pod is healthy if we can query Memgraph role successfully
		healthy := podInfo.MemgraphRole != ""
		if healthy {
			healthyPods++
		} else if podInfo.BoltAddress == "" {
			// Pod without IP is also unhealthy
			healthy = false
		}

		// Pod status will be handled in convertPodInfoToStatus which checks DetectStateInconsistency

		podStatus := convertPodInfoToStatus(podInfo, healthy)
		podStatuses = append(podStatuses, podStatus)
	}

	unhealthyPods := totalPods - healthyPods

	response := &StatusResponse{
		Timestamp: time.Now(),
		ClusterState: ClusterStatus{
			CurrentMaster:         clusterState.CurrentMaster,
			CurrentSyncReplica:    currentSyncReplica,
			TotalPods:             totalPods,
			HealthyPods:           healthyPods,
			UnhealthyPods:         unhealthyPods,
			SyncReplicaHealthy:    syncReplicaHealthy,
			ReconciliationMetrics: c.GetReconciliationMetrics(),
		},
		Pods: podStatuses,
	}

	log.Printf("Cluster status collected: %d total pods, %d healthy, %d unhealthy, master: %s, sync_replica: %s (healthy: %t)", 
		totalPods, healthyPods, unhealthyPods, clusterState.CurrentMaster, currentSyncReplica, syncReplicaHealthy)

	return response, nil
}

// StartHTTPServer starts the HTTP server for status API
func (c *MemgraphController) StartHTTPServer() error {
	if c.httpServer == nil {
		return fmt.Errorf("HTTP server not initialized")
	}
	return c.httpServer.Start()
}

// StopHTTPServer gracefully stops the HTTP server
func (c *MemgraphController) StopHTTPServer(ctx context.Context) error {
	if c.httpServer == nil {
		return nil // Nothing to stop
	}
	return c.httpServer.Stop(ctx)
}

// setupInformers initializes Kubernetes informers for event-driven reconciliation
func (c *MemgraphController) setupInformers() {
	// Create shared informer factory
	c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		c.clientset,
		time.Second*30, // Resync period
		informers.WithNamespace(c.config.Namespace),
	)

	// Set up pod informer with label selector
	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()
	
	// Add event handlers
	c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onPodAdd,
		UpdateFunc: c.onPodUpdate,
		DeleteFunc: c.onPodDelete,
	})
}

// onPodAdd handles pod addition events
func (c *MemgraphController) onPodAdd(obj interface{}) {
	c.enqueuePodEvent("pod-added")
}

// onPodUpdate handles pod update events
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)
	
	if !c.shouldReconcile(oldPod, newPod) {
		return // Skip unnecessary reconciliation
	}
	
	c.enqueuePodEvent("pod-state-changed")
}

// onPodDelete handles pod deletion events
func (c *MemgraphController) onPodDelete(obj interface{}) {
	c.enqueuePodEvent("pod-deleted")
}

// shouldReconcile determines if a pod update requires reconciliation
func (c *MemgraphController) shouldReconcile(oldPod, newPod *v1.Pod) bool {
	// Only reconcile for meaningful changes
	meaningfulChanges := []bool{
		oldPod.Status.Phase != newPod.Status.Phase,           // Lifecycle change
		oldPod.Status.PodIP != newPod.Status.PodIP,           // Network change  
		oldPod.DeletionTimestamp != newPod.DeletionTimestamp, // Deletion started
		oldPod.Spec.NodeName != newPod.Spec.NodeName,         // Node migration
	}
	
	for _, hasChange := range meaningfulChanges {
		if hasChange {
			return true
		}
	}
	
	return false
}

// enqueuePodEvent adds a reconcile request to the work queue
func (c *MemgraphController) enqueuePodEvent(reason string) {
	select {
	case c.workQueue <- reconcileRequest{reason: reason, timestamp: time.Now()}:
		log.Printf("Enqueued reconcile request: %s", reason)
	default:
		log.Printf("Work queue full, dropping reconcile request: %s", reason)
	}
}

// Run starts the main controller loop
func (c *MemgraphController) Run(ctx context.Context) error {
	log.Println("Starting Memgraph Controller main loop...")
	
	c.mu.Lock()
	if c.isRunning {
		c.mu.Unlock()
		return fmt.Errorf("controller is already running")
	}
	c.isRunning = true
	c.mu.Unlock()
	
	// Start informers
	log.Println("Starting Kubernetes informers...")
	c.informerFactory.Start(c.stopCh)
	
	// Wait for informer caches to sync
	log.Println("Waiting for informer caches to sync...")
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer caches")
	}
	log.Println("Informer caches synced successfully")
	
	// Initial reconciliation
	c.enqueueReconcile("initial-reconciliation")
	
	// Start worker goroutines
	const numWorkers = 2
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			c.worker(ctx, workerID)
		}(i)
	}
	
	// Start periodic reconciliation timer
	ticker := time.NewTicker(c.config.ReconcileInterval)
	defer ticker.Stop()
	
	log.Printf("Controller loop started with %d workers, reconcile interval: %s", 
		numWorkers, c.config.ReconcileInterval)
	
	// Main controller loop
	for {
		select {
		case <-ctx.Done():
			log.Println("Context cancelled, stopping controller...")
			c.stop()
			wg.Wait()
			return ctx.Err()
			
		case <-ticker.C:
			c.enqueueReconcile("periodic-reconciliation")
		}
	}
}

// worker processes reconcile requests from the work queue
func (c *MemgraphController) worker(ctx context.Context, workerID int) {
	log.Printf("Worker %d started", workerID)
	
	for {
		select {
		case <-ctx.Done():
			log.Printf("Worker %d stopping due to context cancellation", workerID)
			return
			
		case <-c.stopCh:
			log.Printf("Worker %d stopping due to stop signal", workerID)
			return
			
		case request := <-c.workQueue:
			c.processReconcileRequest(ctx, request, workerID)
		}
	}
}

// processReconcileRequest handles a single reconcile request
func (c *MemgraphController) processReconcileRequest(ctx context.Context, request reconcileRequest, workerID int) {
	startTime := time.Now()
	log.Printf("Worker %d processing reconcile request: %s (queued at %s)", 
		workerID, request.reason, request.timestamp.Format(time.RFC3339))
	
	// Execute reconciliation with retry logic
	err := c.reconcileWithBackoff(ctx)
	duration := time.Since(startTime)
	
	// Update reconciliation metrics
	c.updateReconciliationMetrics(request.reason, duration, err)
	
	c.mu.Lock()
	c.lastReconcile = time.Now()
	if err != nil {
		c.failureCount++
		log.Printf("Worker %d reconciliation failed (%d/%d failures): %v", 
			workerID, c.failureCount, c.maxFailures, err)
		
		// Check if we've exceeded max failures
		if c.failureCount >= c.maxFailures {
			log.Printf("Worker %d: Maximum failures (%d) reached, will attempt recovery on next cycle", 
				workerID, c.maxFailures)
		}
	} else {
		c.failureCount = 0 // Reset failure count on success
		log.Printf("Worker %d reconciliation completed successfully in %s", 
			workerID, duration)
	}
	c.mu.Unlock()
}

// reconcileWithBackoff performs reconciliation with exponential backoff
func (c *MemgraphController) reconcileWithBackoff(ctx context.Context) error {
	const maxRetries = 3
	const baseDelay = time.Second * 2
	
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter
			delay := time.Duration(float64(baseDelay) * math.Pow(2, float64(attempt-1)))
			jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
			totalDelay := delay + jitter
			
			log.Printf("Reconciliation retry %d/%d after %s delay", attempt+1, maxRetries, totalDelay)
			
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(totalDelay):
				// Continue to retry
			}
		}
		
		err := c.Reconcile(ctx)
		if err == nil {
			if attempt > 0 {
				log.Printf("Reconciliation succeeded on attempt %d", attempt+1)
			}
			return nil
		}
		
		log.Printf("Reconciliation attempt %d failed: %v", attempt+1, err)
		
		// Don't retry on certain types of errors
		if c.isNonRetryableError(err) {
			return err
		}
	}
	
	return fmt.Errorf("reconciliation failed after %d attempts", maxRetries)
}

// isNonRetryableError determines if an error should not be retried
func (c *MemgraphController) isNonRetryableError(err error) bool {
	// Don't retry bootstrap safety failures - require manual intervention
	return strings.Contains(err.Error(), "manual intervention required") ||
		   strings.Contains(err.Error(), "ambiguous cluster state")
}

// updateReconciliationMetrics updates the reconciliation metrics
func (c *MemgraphController) updateReconciliationMetrics(reason string, duration time.Duration, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	c.metrics.TotalReconciliations++
	c.metrics.LastReconciliationTime = time.Now()
	c.metrics.LastReconciliationReason = reason
	
	if err == nil {
		c.metrics.SuccessfulReconciliations++
		c.metrics.LastReconciliationError = ""
	} else {
		c.metrics.FailedReconciliations++
		c.metrics.LastReconciliationError = err.Error()
	}
	
	// Update running average (simple moving average)
	if c.metrics.TotalReconciliations == 1 {
		c.metrics.AverageReconciliationTime = duration
	} else {
		c.metrics.AverageReconciliationTime = 
			(c.metrics.AverageReconciliationTime + duration) / 2
	}
}

// GetReconciliationMetrics returns a copy of the current reconciliation metrics
func (c *MemgraphController) GetReconciliationMetrics() ReconciliationMetrics {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	// Return a copy to avoid race conditions
	return *c.metrics
}

// enqueueReconcile adds a reconcile request to the work queue
func (c *MemgraphController) enqueueReconcile(reason string) {
	select {
	case c.workQueue <- reconcileRequest{reason: reason, timestamp: time.Now()}:
		// Successfully enqueued
	default:
		log.Printf("Work queue full, dropping reconcile request: %s", reason)
	}
}

// stop gracefully stops the controller
func (c *MemgraphController) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	if !c.isRunning {
		return
	}
	
	log.Println("Stopping controller...")
	c.isRunning = false
	
	// Signal stop to all components
	close(c.stopCh)
	close(c.workQueue)
	
	log.Println("Controller stopped")
}

// GetControllerStatus returns the current status of the controller
func (c *MemgraphController) GetControllerStatus() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	return map[string]interface{}{
		"running":          c.isRunning,
		"last_reconcile":   c.lastReconcile,
		"failure_count":    c.failureCount,
		"max_failures":     c.maxFailures,
		"work_queue_size":  len(c.workQueue),
	}
}