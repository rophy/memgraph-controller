package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// generateStateConfigMapName creates a ConfigMap name based on the release name
func generateStateConfigMapName() string {
	releaseName := os.Getenv("RELEASE_NAME")
	if releaseName == "" {
		log.Printf("Warning: RELEASE_NAME env var not set, using default ConfigMap name")
		return "memgraph-controller-state"
	}
	return fmt.Sprintf("%s-controller-state", releaseName)
}


type MemgraphController struct {
	clientset      kubernetes.Interface
	config         *Config
	memgraphClient *MemgraphClient
	httpServer     *HTTPServer
	gatewayServer  GatewayServerInterface

	// Leader election
	leaderElection *LeaderElection
	isLeader       bool
	leaderMu       sync.RWMutex

	// State persistence
	stateManager StateManagerInterface

	// Cluster operations
	cluster *MemgraphCluster

	// Controller loop state
	isRunning     bool
	mu            sync.RWMutex
	lastReconcile time.Time
	failureCount  int
	maxFailures   int


	// Event-driven reconciliation
	podInformer       cache.SharedInformer
	configMapInformer cache.SharedInformer
	informerFactory   informers.SharedInformerFactory
	stopCh            chan struct{}

	// Reconciliation metrics
	metrics *ReconciliationMetrics
}

// GatewayServerInterface defines the interface for the gateway server
type GatewayServerInterface interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	SetCurrentMain(endpoint string)
	GetCurrentMain() string
	SetBootstrapPhase(isBootstrap bool)
	IsBootstrapPhase() bool
}

func NewMemgraphController(clientset kubernetes.Interface, config *Config) *MemgraphController {
	controller := &MemgraphController{
		clientset:      clientset,
		config:         config,
		memgraphClient: NewMemgraphClient(config),
	}

	// Initialize HTTP server
	controller.httpServer = NewHTTPServer(controller, config)

	// Initialize gateway server
	gatewayAdapter := NewGatewayAdapter(config)
	if err := gatewayAdapter.InitializeWithMainProvider(controller.GetCurrentMainEndpoint); err != nil {
		log.Printf("Failed to initialize gateway adapter: %v", err)
	}
	controller.gatewayServer = gatewayAdapter

	// Initialize leader election
	controller.leaderElection = NewLeaderElection(clientset, config)
	controller.setupLeaderElectionCallbacks()

	// Initialize state manager with release-based ConfigMap name
	configMapName := generateStateConfigMapName()
	controller.stateManager = NewStateManager(clientset, config.Namespace, configMapName)

	// Initialize cluster operations
	controller.cluster = NewMemgraphCluster(clientset, config, controller.memgraphClient, controller.stateManager)

	// Initialize controller state
	controller.maxFailures = 5
	controller.stopCh = make(chan struct{})

	// Initialize reconciliation metrics
	controller.metrics = &ReconciliationMetrics{}

	// Set up pod informer for event-driven reconciliation
	controller.setupInformers()

	return controller
}

// Delegation methods for backwards compatibility with tests

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
		if len(mainPods) > 0 {
			log.Printf("Current main: %s", mainPods[0])
		}
	}

	// Determine target main index (0 or 1)
	targetMainIndex, err := clusterState.DetermineMainIndex(c.config)
	if err != nil {
		return fmt.Errorf("failed to determine target main index: %w", err)
	}

	// Target main index is now managed in controller state (targetMainIndex)
	log.Printf("Target main index determined: %d (pod: %s)",
		targetMainIndex, c.config.GetPodName(targetMainIndex))

	return nil
}

// applyDeterministicRoles applies roles for fresh/initial clusters
func (c *MemgraphController) applyDeterministicRoles(clusterState *ClusterState) {
	if c.cluster != nil {
		c.cluster.applyDeterministicRoles(clusterState)
	}
}

// learnExistingTopology learns the current operational topology
func (c *MemgraphController) learnExistingTopology(clusterState *ClusterState) {
	if c.cluster != nil {
		c.cluster.learnExistingTopology(clusterState)
	}
}

// selectMainAfterQuerying selects main based on actual Memgraph replication state
func (c *MemgraphController) selectMainAfterQuerying(ctx context.Context, clusterState *ClusterState) {
	if c.cluster != nil {
		c.cluster.selectMainAfterQuerying(ctx, clusterState)
	}
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

// detectMainFailover detects if current main has failed and promotion is needed

func (c *MemgraphController) TestMemgraphConnections(ctx context.Context) error {
	clusterState, err := c.cluster.DiscoverPods(ctx)
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

// Reconcile performs a full reconciliation cycle (simplified operational phase only)
func (c *MemgraphController) Reconcile(ctx context.Context) error {
	log.Println("Starting operational reconciliation cycle...")

	// Discover current cluster state (operational mode only)
	clusterState, err := c.cluster.RefreshClusterInfo(ctx, c.updateSyncReplicaInfo, c.detectMainFailover, c.handleMainFailover)
	if err != nil {
		return fmt.Errorf("failed to discover cluster state: %w", err)
	}

	if len(clusterState.Pods) == 0 {
		log.Println("No Memgraph pods found in cluster")
		return nil
	}

	log.Printf("Current cluster state discovered:")
	log.Printf("  - Total pods: %d", len(clusterState.Pods))
	log.Printf("  - Current main: %s", clusterState.CurrentMain)
	log.Printf("  - Target main index: %d", c.cluster.getTargetMainIndex())

	// Log pod states
	for podName, podInfo := range clusterState.Pods {
		log.Printf("  - Pod %s: State=%s, MemgraphRole=%s, Replicas=%d",
			podName, podInfo.State, podInfo.MemgraphRole, len(podInfo.Replicas))
	}

	// Check for main failover first
	if c.detectMainFailover(clusterState) {
		log.Printf("Main failover detected - initiating failover procedure...")
		if err := c.handleMainFailover(ctx, clusterState); err != nil {
			log.Printf("❌ Main failover failed: %v", err)
			// Continue with topology enforcement to handle partial failures
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

// ConfigureReplication configures main/replica relationships in the cluster


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
	clusterState, err := c.cluster.RefreshClusterInfo(ctx, c.updateSyncReplicaInfo, c.detectMainFailover, c.handleMainFailover)
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
			CurrentMain:           clusterState.CurrentMain,
			CurrentSyncReplica:    currentSyncReplica,
			TotalPods:             totalPods,
			HealthyPods:           healthyPods,
			UnhealthyPods:         unhealthyPods,
			SyncReplicaHealthy:    syncReplicaHealthy,
			IsLeader:              c.IsLeader(),
			ReconciliationMetrics: c.GetReconciliationMetrics(),
		},
		Pods: podStatuses,
	}

	log.Printf("Cluster status collected: %d total pods, %d healthy, %d unhealthy, main: %s, sync_replica: %s (healthy: %t)",
		totalPods, healthyPods, unhealthyPods, clusterState.CurrentMain, currentSyncReplica, syncReplicaHealthy)

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

	// Add event handlers for pods
	c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onPodAdd,
		UpdateFunc: c.onPodUpdate,
		DeleteFunc: c.onPodDelete,
	})

	// Set up ConfigMap informer for controller state synchronization
	c.configMapInformer = c.informerFactory.Core().V1().ConfigMaps().Informer()

	// Add event handlers for ConfigMaps
	c.configMapInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onConfigMapAdd,
		UpdateFunc: c.onConfigMapUpdate,
		DeleteFunc: c.onConfigMapDelete,
	})
}

// setupLeaderElectionCallbacks configures the callbacks for leader election
func (c *MemgraphController) setupLeaderElectionCallbacks() {
	c.leaderElection.SetCallbacks(
		func(ctx context.Context) {
			// OnStartedLeading: This controller instance became leader
			c.leaderMu.Lock()
			c.isLeader = true
			c.leaderMu.Unlock()
			
			log.Println("🎯 Became leader - loading state and starting controller operations")
			
			// Load state to determine startup phase (BOOTSTRAP vs OPERATIONAL)
			if err := c.loadControllerStateOnStartup(ctx); err != nil {
				log.Printf("Warning: Failed to load startup state: %v", err)
			}
		},
		func() {
			// OnStoppedLeading: This controller instance lost leadership
			c.leaderMu.Lock()
			c.isLeader = false
			c.leaderMu.Unlock()
			
			log.Println("⏹️  Lost leadership - stopping operations")
			// Stop reconciliation operations but keep the process running
		},
		func(identity string) {
			// OnNewLeader: A new leader was elected
			log.Printf("👑 New leader elected: %s", identity)
		},
	)
}

// IsLeader returns true if this controller instance is the current leader
func (c *MemgraphController) IsLeader() bool {
	c.leaderMu.RLock()
	defer c.leaderMu.RUnlock()
	return c.isLeader
}

// GetLeaderElection returns the leader election instance for API access
func (c *MemgraphController) GetLeaderElection() *LeaderElection {
	return c.leaderElection
}

// saveControllerStateAfterBootstrap persists the controller state after successful bootstrap

// loadControllerStateOnStartup loads persisted state and determines startup phase
func (c *MemgraphController) loadControllerStateOnStartup(ctx context.Context) error {
	exists, err := c.stateManager.StateExists(ctx)
	if err != nil {
		return fmt.Errorf("failed to check state existence: %w", err)
	}

	if !exists {
		log.Println("No state ConfigMap found - will start in BOOTSTRAP phase")
		return nil // Keep default bootstrap=true
	}

	state, err := c.stateManager.LoadState(ctx)
	if err != nil {
		log.Printf("Warning: Failed to load state ConfigMap: %v", err)
		log.Println("Will start in BOOTSTRAP phase as fallback")
		return nil // Don't fail startup, just use bootstrap
	}

	// If state exists, assume bootstrap is completed and start in OPERATIONAL phase

	// Transition gateway to operational phase for non-leaders per DESIGN.md
	if c.gatewayServer != nil {
		c.gatewayServer.SetBootstrapPhase(false)
		log.Printf("Gateway transitioned to operational phase (ConfigMap indicates bootstrap completed)")
	}

	log.Printf("Loaded persisted state - will start in OPERATIONAL phase: masterIndex=%d", state.MasterIndex)

	return nil
}

// getTargetMainIndex returns the current target main index from state manager
func (c *MemgraphController) getTargetMainIndex() int {
	// Handle test cases where stateManager is nil
	if c.stateManager == nil {
		return -1 // Bootstrap phase / test scenario
	}
	
	ctx := context.Background()
	state, err := c.stateManager.LoadState(ctx)
	if err != nil {
		// Return -1 if no state available (bootstrap phase)
		return -1
	}
	return state.MasterIndex
}

// getCurrentMainFromState gets the current main pod name from state manager
func (c *MemgraphController) getCurrentMainFromState(ctx context.Context) (string, error) {
	// Handle test cases where stateManager is nil
	if c.stateManager == nil {
		return "", fmt.Errorf("no state manager available")
	}
	
	state, err := c.stateManager.LoadState(ctx)
	if err != nil {
		return "", err
	}
	return c.config.GetPodName(state.MasterIndex), nil
}

// updateTargetMainIndex updates target main index in state manager
func (c *MemgraphController) updateTargetMainIndex(ctx context.Context, newTargetIndex int, reason string) error {
	if c.cluster != nil {
		return c.cluster.updateTargetMainIndex(ctx, newTargetIndex, reason)
	}
	return fmt.Errorf("cluster not initialized")
}

// onPodAdd handles pod addition events
func (c *MemgraphController) onPodAdd(obj interface{}) {
	c.enqueuePodEvent("pod-added")
}

// onPodUpdate handles pod update events
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// Check for IP changes and update connection pool
	if oldPod.Status.PodIP != newPod.Status.PodIP && newPod.Status.PodIP != "" {
		c.memgraphClient.connectionPool.UpdatePodIP(newPod.Name, newPod.Status.PodIP)
		log.Printf("Pod %s IP changed: %s -> %s", newPod.Name, oldPod.Status.PodIP, newPod.Status.PodIP)
	}

	if !c.shouldReconcile(oldPod, newPod) {
		return // Skip unnecessary reconciliation
	}

	c.enqueuePodEvent("pod-state-changed")
}

// onPodDelete handles pod deletion events
func (c *MemgraphController) onPodDelete(obj interface{}) {
	pod := obj.(*v1.Pod)
	
	// Invalidate connection for deleted pod
	c.memgraphClient.connectionPool.InvalidatePodConnection(pod.Name)
	log.Printf("Pod %s deleted, invalidated connection", pod.Name)

	// Check if the deleted pod is the current main - trigger IMMEDIATE failover
	ctx := context.Background()
	currentMain, err := c.getCurrentMainFromState(ctx)
	if err != nil {
		log.Printf("Could not get current main from state: %v", err)
		currentMain = ""
	}
	
	if currentMain != "" && pod.Name == currentMain {
		log.Printf("🚨 MAIN POD DELETED: %s - triggering IMMEDIATE failover", pod.Name)
		
		// Only leader should handle failover
		if c.IsLeader() {
			go c.handleImmediateFailover(pod.Name)
		} else {
			log.Printf("Non-leader detected main deletion - leader will handle failover")
		}
	}

	// Still enqueue for reconciliation cleanup
	c.enqueuePodEvent("pod-deleted")
}

// onConfigMapAdd handles ConfigMap creation events
func (c *MemgraphController) onConfigMapAdd(obj interface{}) {
	configMap := obj.(*v1.ConfigMap)
	
	// Only process our controller state ConfigMap
	if configMap.Name != c.stateManager.ConfigMapName() {
		return
	}
	
	log.Printf("🔄 ConfigMap added: %s", configMap.Name)
	
	// Extract masterIndex from ConfigMap
	masterIndexStr, exists := configMap.Data["masterIndex"]
	if !exists {
		log.Printf("Warning: ConfigMap %s missing masterIndex", configMap.Name)
		return
	}
	
	// Parse masterIndex
	var newMasterIndex int
	if _, err := fmt.Sscanf(masterIndexStr, "%d", &newMasterIndex); err != nil {
		log.Printf("Failed to parse masterIndex from ConfigMap: %v", err)
		return
	}
	
	// Check if this is different from our current state
	currentIndex := c.cluster.getTargetMainIndex()
	
	if currentIndex != newMasterIndex {
		log.Printf("Target main index changed: %d -> %d", currentIndex, newMasterIndex)
		c.handleTargetMainChanged(newMasterIndex)
	}
	
	// Transition gateway to operational phase for non-leaders
	if !c.IsLeader() && c.gatewayServer != nil {
		c.gatewayServer.SetBootstrapPhase(false)
		log.Printf("✅ Gateway transitioned to operational phase (ConfigMap created, bootstrap complete)")
	}
}

// onConfigMapUpdate handles ConfigMap update events
func (c *MemgraphController) onConfigMapUpdate(oldObj, newObj interface{}) {
	newConfigMap := newObj.(*v1.ConfigMap)
	
	// Only process our controller state ConfigMap
	if newConfigMap.Name != c.stateManager.ConfigMapName() {
		return
	}
	
	// Extract new masterIndex from ConfigMap
	newMasterIndexStr, exists := newConfigMap.Data["masterIndex"]
	if !exists {
		log.Printf("Warning: ConfigMap %s missing masterIndex", newConfigMap.Name)
		return
	}
	
	// Parse new masterIndex
	var newMasterIndex int
	if _, err := fmt.Sscanf(newMasterIndexStr, "%d", &newMasterIndex); err != nil {
		log.Printf("Failed to parse masterIndex from ConfigMap: %v", err)
		return
	}
	
	// Compare with our current state (not old ConfigMap)
	currentIndex := c.cluster.getTargetMainIndex()
	
	if currentIndex == newMasterIndex {
		// No change from our perspective, ignore
		return
	}
	
	log.Printf("🔄 ConfigMap state change detected: MasterIndex %d -> %d", currentIndex, newMasterIndex)
	c.handleTargetMainChanged(newMasterIndex)
}

// onConfigMapDelete handles ConfigMap deletion events
func (c *MemgraphController) onConfigMapDelete(obj interface{}) {
	configMap := obj.(*v1.ConfigMap)
	
	// Only process our controller state ConfigMap
	if configMap.Name != c.stateManager.ConfigMapName() {
		return
	}
	
	log.Printf("⚠️ ConfigMap deleted: %s", configMap.Name)
}

// handleTargetMainChanged processes target main index changes and coordinates controller pods
func (c *MemgraphController) handleTargetMainChanged(newMasterIndex int) {
	coordinationStartTime := time.Now()
	
	// Don't process our own changes if we're the leader
	if c.IsLeader() {
		log.Printf("⚠️ Leader ignoring target main change (likely caused by own update)")
		return
	}
	
	oldTargetIndex := c.cluster.getTargetMainIndex()
	
	oldLastKnownMain := ""
	if oldTargetIndex >= 0 {
		oldLastKnownMain = c.config.GetPodName(oldTargetIndex)
	}
	
	// Check if this is actually a change
	if oldTargetIndex == newMasterIndex {
		log.Printf("📋 Target main index unchanged (%d), ignoring", newMasterIndex)
		return
	}
	
	// Log detailed coordination information
	log.Printf("🔄 COORDINATION EVENT: Non-leader detected MasterIndex change")
	log.Printf("   📊 Change: %d -> %d", oldTargetIndex, newMasterIndex)
	log.Printf("   🏷️ Pod Identity: %s", c.getPodIdentity())
	
	// Phase 1: Update local controller state atomically
	newLastKnownMain := ""
	if newMasterIndex >= 0 {
		newLastKnownMain = c.config.GetPodName(newMasterIndex)
	}
	
	// Update state directly via state manager (for non-leaders)
	
	log.Printf("   ✅ State updated: targetMainIndex=%d, lastKnownMain=%s -> %s", 
		newMasterIndex, oldLastKnownMain, newLastKnownMain)
	
	// Phase 2: Coordinate gateway connection termination
	connectionTerminationTime := time.Now()
	
	if c.gatewayServer != nil {
		log.Printf("   🔄 Coordinating gateway connection termination...")
		
		// Get current connection count before termination
		// Note: This assumes gateway has a method to get connection count
		// We'll log the termination attempt for now
		
		endpoint, err := c.GetCurrentMainEndpoint(context.Background())
		if err != nil {
			log.Printf("   ⚠️ Failed to get current main endpoint: %v", err)
			endpoint = "" // Use empty endpoint to trigger connection termination
		}
		
		log.Printf("   🎯 New main endpoint: %s", endpoint)
		c.gatewayServer.SetCurrentMain(endpoint)
		
		// The SetCurrentMain call will trigger terminateAllConnections if main changed
		log.Printf("   ✅ Gateway updated with new main endpoint")
	} else {
		log.Printf("   ⚠️ Gateway server not available for coordination")
	}
	
	// Phase 3: Coordination completion logging
	coordinationDuration := time.Since(coordinationStartTime)
	terminationDuration := time.Since(connectionTerminationTime)
	
	log.Printf("🎯 COORDINATION COMPLETED:")
	log.Printf("   📈 Total duration: %v", coordinationDuration)
	log.Printf("   🔌 Connection handling: %v", terminationDuration)
	log.Printf("   🏷️ Final state: targetMainIndex=%d, lastKnownMain=%s", 
		newMasterIndex, newLastKnownMain)
	log.Printf("   🕒 Coordinated at: %s", time.Now().Format(time.RFC3339))
}

// getPodIdentity returns the current pod's identity for coordination logging
func (c *MemgraphController) getPodIdentity() string {
	// Try to get pod name from environment or default to hostname-based
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName = "unknown-pod"
	}
	
	leaderStatus := "non-leader"
	if c.IsLeader() {
		leaderStatus = "leader"
	}
	
	return fmt.Sprintf("%s (%s)", podName, leaderStatus)
}

// RefreshPodInfo gets fresh pod information from Kubernetes API and updates connection pool
func (c *MemgraphController) RefreshPodInfo(ctx context.Context, podName string) (*PodInfo, error) {
	pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get fresh pod info for %s: %w", podName, err)
	}

	// Update connection pool with fresh IP
	if pod.Status.PodIP != "" {
		c.memgraphClient.connectionPool.UpdatePodIP(podName, pod.Status.PodIP)
		log.Printf("Refreshed pod %s IP: %s", podName, pod.Status.PodIP)
	}

	return NewPodInfo(pod), nil
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

// enqueuePodEvent logs pod events - reconciliation now happens on timer
func (c *MemgraphController) enqueuePodEvent(reason string) {
	log.Printf("Pod event detected: %s (reconciliation will occur on next timer cycle)", reason)
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

	// STEP 1: Setup informers, HTTP server, gateway, leader election
	log.Println("Setting up controller components...")

	// Start informers
	log.Println("Starting Kubernetes informers...")
	c.informerFactory.Start(c.stopCh)

	// Wait for informer caches to sync
	log.Println("Waiting for informer caches to sync...")
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced, c.configMapInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer caches")
	}
	log.Println("Informer caches synced successfully")

	// Start HTTP server for status API
	if c.httpServer != nil {
		log.Println("Starting HTTP server...")
		if err := c.httpServer.Start(); err != nil {
			log.Printf("Failed to start HTTP server: %v", err)
			c.stop()
			return fmt.Errorf("failed to start HTTP server: %w", err)
		}
		log.Println("HTTP server started successfully")
	}

	// Start gateway server
	if c.gatewayServer != nil {
		log.Println("Starting gateway server...")
		if err := c.gatewayServer.Start(ctx); err != nil {
			log.Printf("Failed to start gateway server: %v", err)
			c.stop()
			return fmt.Errorf("failed to start gateway server: %w", err)
		}
		log.Println("Gateway server started successfully")
	}

	// Start leader election in goroutine
	log.Println("Starting leader election...")
	go func() {
		if err := c.leaderElection.Run(ctx); err != nil {
			log.Printf("Leader election failed: %v", err)
		}
	}()
	log.Println("Leader election started successfully")

	// STEP 2: Start reconciliation loop
	log.Println("Starting reconciliation loop...")

	// Start periodic reconciliation timer
	ticker := time.NewTicker(c.config.ReconcileInterval)
	defer ticker.Stop()

	log.Printf("Controller setup complete, starting reconciliation loop with interval: %s", c.config.ReconcileInterval)

	// Main reconciliation loop - implements DESIGN.md simplified flow
	for {
		select {
		case <-ctx.Done():
			log.Println("Context cancelled, stopping controller...")
			c.stop()
			return ctx.Err()

		case <-ticker.C:
			// Reconciliation logic per DESIGN.md:
			// if leader:
			//   if configmap not ready then discoverClusterAndCreateConfigMap()
			//   reconcile()
			// else (non-leader): noop
			if c.IsLeader() {
				if err := c.performLeaderReconciliation(ctx); err != nil {
					log.Printf("Leader reconciliation failed: %v", err)
				}
			} else {
				log.Printf("Not leader, skipping reconciliation cycle")
			}
		}
	}
}

// performLeaderReconciliation implements the simplified DESIGN.md reconciliation logic
func (c *MemgraphController) performLeaderReconciliation(ctx context.Context) error {
	log.Println("Performing leader reconciliation...")

	// Check if ConfigMap is ready (exists and has valid state)
	configMapReady, err := c.isConfigMapReady(ctx)
	if err != nil {
		return fmt.Errorf("failed to check ConfigMap readiness: %w", err)
	}

	if !configMapReady {
		log.Println("ConfigMap not ready - performing discovery and creating ConfigMap...")
		if err := c.discoverClusterAndCreateConfigMap(ctx); err != nil {
			return fmt.Errorf("failed to discover cluster and create ConfigMap: %w", err)
		}
		log.Println("✅ Cluster discovered and ConfigMap created")
	}

	// Perform regular reconciliation
	log.Println("Performing reconciliation...")
	if err := c.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconciliation failed: %w", err)
	}

	log.Println("✅ Leader reconciliation completed successfully")
	return nil
}

// isConfigMapReady checks if the controller state ConfigMap exists and is valid
func (c *MemgraphController) isConfigMapReady(ctx context.Context) (bool, error) {
	state, err := c.stateManager.LoadState(ctx)
	if err != nil {
		// ConfigMap doesn't exist or is invalid
		log.Printf("ConfigMap not ready: %v", err)
		return false, nil
	}

	// Validate that the state contains required information
	if state.MasterIndex < 0 || state.MasterIndex > 1 {
		log.Printf("ConfigMap contains invalid MasterIndex: %d", state.MasterIndex)
		return false, nil
	}

	log.Printf("ConfigMap is ready with MasterIndex: %d", state.MasterIndex)
	return true, nil
}

// discoverClusterAndCreateConfigMap implements DESIGN.md "Discover Cluster State" section
func (c *MemgraphController) discoverClusterAndCreateConfigMap(ctx context.Context) error {
	log.Println("=== CLUSTER DISCOVERY ===")

	// Use bootstrap logic to discover and initialize cluster
	bootstrapController := NewBootstrapController(c)
	clusterState, err := bootstrapController.ExecuteBootstrap(ctx)
	if err != nil {
		return fmt.Errorf("cluster discovery failed: %w", err)
	}

	log.Printf("✅ Cluster discovery completed - main: %s", clusterState.CurrentMain)
	return nil
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


// stop gracefully stops the controller
func (c *MemgraphController) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.isRunning {
		return
	}

	log.Println("Stopping controller...")
	c.isRunning = false

	// Stop gateway server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.gatewayServer.Stop(shutdownCtx); err != nil {
		log.Printf("Error stopping gateway server: %v", err)
	}

	// Signal stop to all components
	close(c.stopCh)

	log.Println("Controller stopped")
}

// GetControllerStatus returns the current status of the controller
func (c *MemgraphController) GetControllerStatus() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return map[string]interface{}{
		"running":         c.isRunning,
		"last_reconcile":  c.lastReconcile,
		"failure_count":   c.failureCount,
		"max_failures":    c.maxFailures,
	}
}

// GetCurrentMainEndpoint returns the current main endpoint for gateway connections
func (c *MemgraphController) GetCurrentMainEndpoint(ctx context.Context) (string, error) {
	var currentMain string
	
	// Both leaders and non-leaders read from ConfigMap to ensure consistency
	currentMain, err := c.getCurrentMainFromState(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read current main from state: %w", err)
	}

	if currentMain == "" {
		return "", fmt.Errorf("no current main known")
	}

	// Get the pod to find its IP address
	pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, currentMain, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get main pod %s: %w", currentMain, err)
	}

	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("main pod %s has no IP address", currentMain)
	}

	endpoint := fmt.Sprintf("%s:%s", pod.Status.PodIP, c.config.BoltPort)
	return endpoint, nil
}

// updateGatewayMain notifies the gateway of main changes

// handleImmediateFailover handles immediate failover triggered by pod deletion events
func (c *MemgraphController) handleImmediateFailover(deletedPodName string) {
	failoverStartTime := time.Now()
	log.Printf("🚀 IMMEDIATE FAILOVER TRIGGERED: Main pod %s deleted", deletedPodName)
	
	// Create context with timeout for failover operations
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	
	// Discover cluster state to find SYNC replica
	log.Printf("🔍 Discovering cluster state for immediate failover...")
	clusterState, err := c.cluster.RefreshClusterInfo(ctx, c.updateSyncReplicaInfo, c.detectMainFailover, c.handleMainFailover)
	if err != nil {
		log.Printf("❌ CRITICAL: Failed to discover cluster state for immediate failover: %v", err)
		return
	}
	
	// Log cluster state
	log.Printf("📊 Immediate failover cluster state: %d pods, main=%s", 
		len(clusterState.Pods), clusterState.CurrentMain)
	
	// Execute immediate failover using existing logic
	if err := c.handleMainFailover(ctx, clusterState); err != nil {
		log.Printf("❌ IMMEDIATE FAILOVER FAILED: %v", err)
		return
	}
	
	totalFailoverTime := time.Since(failoverStartTime)
	log.Printf("🎯 IMMEDIATE FAILOVER COMPLETED in %v: Event-driven failover successful", 
		totalFailoverTime)
}

// handleMainFailurePromotion promotes SYNC replica when main fails during operations

// promoteToMain promotes a pod to main role



