package controller

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

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

	log.Printf("Discovered %d pods, current master: %s", len(clusterState.Pods), clusterState.CurrentMaster)

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

// selectMasterAfterQuerying selects master based on actual Memgraph replication state
func (c *MemgraphController) selectMasterAfterQuerying(clusterState *ClusterState) {
	// Priority-based master selection using ACTUAL Memgraph state
	// 1. Prefer existing MAIN node (avoid unnecessary failover)
	// 2. If no MAIN, promote SYNC replica (guaranteed consistency)  
	// 3. If no SYNC replica, use timestamp fallback (with warnings)
	
	var existingMain *PodInfo
	var syncReplica *PodInfo
	var latestPod *PodInfo
	var latestTime time.Time

	// Analyze all pods based on ACTUAL Memgraph roles
	for _, podInfo := range clusterState.Pods {
		// Priority 1: Existing MAIN node (from actual Memgraph query)
		if podInfo.MemgraphRole == "main" {
			existingMain = podInfo
			log.Printf("Found existing MAIN node: %s (actual role, not label)", podInfo.Name)
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

	// Master selection decision tree (based on actual state)
	var selectedMaster *PodInfo
	var selectionReason string
	
	if existingMain != nil {
		// Prefer existing MAIN node - avoid unnecessary failover
		selectedMaster = existingMain
		selectionReason = "existing MAIN node (from actual Memgraph state)"
	} else if syncReplica != nil {
		// ONLY safe automatic promotion: SYNC replica has all committed data
		selectedMaster = syncReplica
		selectionReason = "SYNC replica promotion (guaranteed consistency)"
		log.Printf("PROMOTING SYNC REPLICA: %s has all committed transactions", syncReplica.Name)
	} else {
		// No SYNC replica available - use timestamp fallback but log warning
		selectedMaster = latestPod
		selectionReason = "latest timestamp fallback (ASYNC replicas may be missing data)"
		if len(clusterState.Pods) > 1 {
			log.Printf("WARNING: No SYNC replica available for safe promotion")
			log.Printf("WARNING: Selected master %s may be missing committed transactions", latestPod.Name)
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
	
	// Log pod states
	for podName, podInfo := range clusterState.Pods {
		log.Printf("  - Pod %s: State=%s, K8sRole=%s, MemgraphRole=%s, Replicas=%d", 
			podName, podInfo.State, podInfo.KubernetesRole, podInfo.MemgraphRole, len(podInfo.Replicas))
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

	// Phase 2: Configure SYNC/ASYNC replication strategy
	if err := c.configureReplicationWithSyncStrategy(ctx, clusterState); err != nil {
		log.Printf("Failed to configure SYNC/ASYNC replication: %v", err)
		configErrors = append(configErrors, fmt.Errorf("SYNC/ASYNC replication configuration: %w", err))
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

// selectSyncReplica determines which replica should be configured as SYNC
// Uses deterministic selection: first pod alphabetically (e.g., memgraph-0 over memgraph-1)
func (c *MemgraphController) selectSyncReplica(clusterState *ClusterState, currentMaster string) string {
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
	
	// Return first pod alphabetically
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

// configureReplicationWithSyncStrategy configures replication using SYNC/ASYNC strategy
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

// promoteAsyncToSync promotes an ASYNC replica to SYNC mode (emergency procedure)
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
	
	log.Printf("EMERGENCY: Promoting ASYNC replica %s to SYNC mode", targetReplicaPod)
	
	replicaName := targetPod.GetReplicaName()
	replicaAddress := targetPod.GetReplicationAddress(c.config.ServiceName)
	
	// Step 1: Drop existing ASYNC registration
	log.Printf("Dropping existing ASYNC replica registration: %s", replicaName)
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replicaName); err != nil {
		return fmt.Errorf("failed to drop ASYNC replica %s: %w", replicaName, err)
	}
	
	// Step 2: Re-register as SYNC replica
	log.Printf("Re-registering %s as SYNC replica", replicaName)
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("failed to register SYNC replica %s: %w", replicaName, err)
	}
	
	// Step 3: Update tracking
	targetPod.IsSyncReplica = true
	
	log.Printf("Successfully promoted %s to SYNC replica", targetReplicaPod)
	log.Printf("WARNING: Verify replica is caught up before considering promotion complete")
	
	return nil
}

// handleSyncReplicaFailure responds to SYNC replica becoming unavailable
func (c *MemgraphController) handleSyncReplicaFailure(ctx context.Context, clusterState *ClusterState, failedSyncReplica string) error {
	currentMaster := clusterState.CurrentMaster
	if currentMaster == "" {
		return fmt.Errorf("no master available during SYNC replica failure")
	}
	
	log.Printf("CRITICAL: SYNC replica %s is down - master will block all writes", failedSyncReplica)
	log.Printf("Available options:")
	log.Printf("  1. Restart SYNC replica pod (preferred)")
	log.Printf("  2. Promote healthy ASYNC replica to SYNC")
	log.Printf("  3. Drop SYNC replica (accept data loss risk)")
	
	// Find healthy ASYNC replicas for potential promotion
	var healthyAsyncReplicas []string
	for podName, podInfo := range clusterState.Pods {
		if podName != currentMaster && podName != failedSyncReplica && !podInfo.IsSyncReplica {
			// Check if pod is healthy (has bolt address and can be reached)
			if podInfo.BoltAddress != "" && podInfo.MemgraphRole == "replica" {
				healthyAsyncReplicas = append(healthyAsyncReplicas, podName)
			}
		}
	}
	
	if len(healthyAsyncReplicas) == 0 {
		log.Printf("CRITICAL: No healthy ASYNC replicas available for emergency promotion")
		return fmt.Errorf("no healthy ASYNC replicas for emergency SYNC promotion")
	}
	
	// Select first healthy ASYNC replica alphabetically (deterministic)
	for i := 0; i < len(healthyAsyncReplicas)-1; i++ {
		for j := i + 1; j < len(healthyAsyncReplicas); j++ {
			if healthyAsyncReplicas[i] > healthyAsyncReplicas[j] {
				healthyAsyncReplicas[i], healthyAsyncReplicas[j] = healthyAsyncReplicas[j], healthyAsyncReplicas[i]
			}
		}
	}
	
	emergencyCandidate := healthyAsyncReplicas[0]
	log.Printf("Emergency SYNC candidate: %s", emergencyCandidate)
	
	// Note: This is just identification - actual promotion should be manual or with explicit user confirmation
	log.Printf("To restore write capability, run emergency promotion:")
	log.Printf("  kubectl exec %s -- bash -c 'echo \"DROP REPLICA %s; REGISTER REPLICA %s SYNC TO \\\"%s:10000\\\";\" | mgconsole'", 
		currentMaster, strings.ReplaceAll(failedSyncReplica, "-", "_"), 
		strings.ReplaceAll(emergencyCandidate, "-", "_"), emergencyCandidate)
	
	return nil
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
		log.Printf("No SYNC replica configured - cluster may have write availability issues")
		return nil
	}
	
	// Check SYNC replica health
	syncPod := clusterState.Pods[syncReplicaPod]
	if syncPod.BoltAddress == "" || syncPod.MemgraphRole == "" {
		log.Printf("SYNC replica %s appears unhealthy - investigating", syncReplicaPod)
		
		// Try to connect to SYNC replica
		if err := c.memgraphClient.TestConnectionWithRetry(ctx, syncPod.BoltAddress); err != nil {
			log.Printf("CRITICAL: SYNC replica %s is unreachable: %v", syncReplicaPod, err)
			return c.handleSyncReplicaFailure(ctx, clusterState, syncReplicaPod)
		}
	}
	
	log.Printf("SYNC replica %s is healthy", syncReplicaPod)
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
		log.Printf("Pod %s final state: %s (K8sRole=%s, MemgraphRole=%s)", 
			podName, podInfo.State, podInfo.KubernetesRole, podInfo.MemgraphRole)
	}

	// Use the pod discovery component to sync labels
	err := c.podDiscovery.SyncPodLabelsWithState(ctx, clusterState)
	if err != nil {
		return fmt.Errorf("failed to synchronize pod labels with state: %w", err)
	}

	log.Println("Pod label synchronization completed successfully")
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
			CurrentMaster:      clusterState.CurrentMaster,
			CurrentSyncReplica: currentSyncReplica,
			TotalPods:          totalPods,
			HealthyPods:        healthyPods,
			UnhealthyPods:      unhealthyPods,
			SyncReplicaHealthy: syncReplicaHealthy,
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
	c.enqueuePodEvent("pod-updated")
}

// onPodDelete handles pod deletion events
func (c *MemgraphController) onPodDelete(obj interface{}) {
	c.enqueuePodEvent("pod-deleted")
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
			workerID, time.Since(startTime))
	}
	c.mu.Unlock()
}

// reconcileWithBackoff performs reconciliation with exponential backoff
func (c *MemgraphController) reconcileWithBackoff(ctx context.Context) error {
	const maxRetries = 3
	const baseDelay = time.Second * 2
	
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Calculate exponential backoff delay
			delay := time.Duration(float64(baseDelay) * math.Pow(2, float64(attempt-1)))
			log.Printf("Reconciliation attempt %d failed, retrying in %s", attempt, delay)
			
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
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
	}
	
	return fmt.Errorf("reconciliation failed after %d attempts", maxRetries)
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