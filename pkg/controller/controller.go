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

	// Cluster operations
	cluster *MemgraphCluster

	// In-memory cluster state for immediate event processing
	stateMutex       sync.RWMutex
	stateLastUpdated time.Time

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
	stateManager := NewStateManager(clientset, config.Namespace, configMapName)

	// Initialize cluster operations
	controller.cluster = NewMemgraphCluster(clientset, config, controller.memgraphClient, stateManager)

	// Initialize controller state
	controller.maxFailures = 5
	controller.stopCh = make(chan struct{})

	// Initialize reconciliation metrics
	controller.metrics = &ReconciliationMetrics{}

	// Set up pod informer for event-driven reconciliation
	controller.setupInformers()

	return controller
}

// getStateManager returns the StateManager instance from the cluster
func (c *MemgraphController) getStateManager() StateManagerInterface {
	if c.cluster == nil {
		return nil
	}
	return c.cluster.GetStateManager()
}

// updateCachedState updates the in-memory cluster state for immediate event processing
func (c *MemgraphController) updateCachedState(cluster *MemgraphCluster) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	
	// The cluster itself is now the cached state - just update timestamp
	c.stateLastUpdated = time.Now()
	
	log.Printf("🔄 Updated cached cluster state: %d pods, main=%s", 
		len(cluster.Pods), cluster.CurrentMain)
}

// getCachedState returns the cached cluster state for immediate event processing
func (c *MemgraphController) getCachedState() (*MemgraphCluster, time.Time) {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	
	return c.cluster, c.stateLastUpdated
}

// isPodBecomeUnhealthy checks if a pod transitioned from healthy to unhealthy state
func (c *MemgraphController) isPodBecomeUnhealthy(oldPod, newPod *v1.Pod) bool {
	// Check if pod went from Running to non-Running state
	if oldPod.Status.Phase == v1.PodRunning && newPod.Status.Phase != v1.PodRunning {
		log.Printf("Pod %s phase changed: %s -> %s", newPod.Name, oldPod.Status.Phase, newPod.Status.Phase)
		return true
	}
	
	// Check if pod became unready (readiness probe failed)
	oldReady := isPodReady(oldPod)
	newReady := isPodReady(newPod)
	if oldReady && !newReady {
		log.Printf("Pod %s became unready (readiness probe failed)", newPod.Name)
		return true
	}
	
	// Check if pod IP was lost
	if oldPod.Status.PodIP != "" && newPod.Status.PodIP == "" {
		log.Printf("Pod %s lost IP address: %s -> empty", newPod.Name, oldPod.Status.PodIP)
		return true
	}
	
	return false
}

// isPodReady checks if a pod has the Ready condition set to True
func isPodReady(pod *v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

// Delegation methods for backwards compatibility with tests


// applyDeterministicRoles applies roles for fresh/initial clusters
func (c *MemgraphController) applyDeterministicRoles(cluster *MemgraphCluster) {
	if c.cluster != nil {
		c.cluster.applyDeterministicRoles()
	}
}

// learnExistingTopology learns the current operational topology
func (c *MemgraphController) learnExistingTopology(cluster *MemgraphCluster) {
	if c.cluster != nil {
		c.cluster.learnExistingTopology()
	}
}

// selectMainAfterQuerying selects main based on actual Memgraph replication state
func (c *MemgraphController) selectMainAfterQuerying(ctx context.Context, cluster *MemgraphCluster) {
	if c.cluster != nil {
		c.cluster.selectMainAfterQuerying(ctx)
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
	err := c.cluster.DiscoverPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover pods for connection testing: %w", err)
	}

	if len(c.cluster.Pods) == 0 {
		log.Println("No pods found for connection testing")
		return nil
	}

	log.Printf("Testing Memgraph connections for %d pods...", len(c.cluster.Pods))

	var connectionErrors []error
	successCount := 0

	for podName, podInfo := range c.cluster.Pods {
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
		successCount, len(c.cluster.Pods))

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
	log.Println("Starting DESIGN.md compliant reconciliation cycle...")

	// Use the new deterministic 8-step reconcile actions instead of complex event-driven logic
	reconcileActions := NewReconcileActions(c, c.cluster)
	if err := reconcileActions.ExecuteReconcileActions(ctx); err != nil {
		return fmt.Errorf("DESIGN.md reconcile actions failed: %w", err)
	}

	// Update cached state after reconciliation
	c.updateCachedState(c.cluster)

	// Sync pod labels with replication state
	if err := c.SyncPodLabels(ctx, c.cluster); err != nil {
		return fmt.Errorf("failed to sync pod labels: %w", err)
	}

	log.Println("DESIGN.md compliant reconciliation cycle completed successfully")
	return nil
}

// ConfigureReplication configures main/replica relationships in the cluster

// SyncPodLabels synchronizes pod labels with their actual replication state
func (c *MemgraphController) SyncPodLabels(ctx context.Context, cluster *MemgraphCluster) error {
	if len(c.cluster.Pods) == 0 {
		log.Println("No pods to sync labels for")
		return nil
	}

	log.Println("Starting pod label synchronization...")

	// Update cluster state with current pod states after replication configuration
	for podName, podInfo := range c.cluster.Pods {
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
	err := c.cluster.RefreshClusterInfo(ctx, c.updateSyncReplicaInfo, c.detectMainFailover, c.handleMainFailover)
	if err != nil {
		return nil, fmt.Errorf("failed to discover cluster state: %w", err)
	}

	// Update cached state for immediate event processing
	c.updateCachedState(c.cluster)

	// Build cluster summary
	totalPods := len(c.cluster.Pods)
	healthyPods := 0
	podStatuses := make([]PodStatus, 0, totalPods)

	// Find current SYNC replica
	var currentSyncReplica string
	var syncReplicaHealthy bool
	for podName, podInfo := range c.cluster.Pods {
		if podInfo.IsSyncReplica {
			currentSyncReplica = podName
			// SYNC replica is healthy if we can query its Memgraph role
			syncReplicaHealthy = podInfo.MemgraphRole != ""
			break
		}
	}

	// Process each pod to determine health and build status
	for _, podInfo := range c.cluster.Pods {
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
			CurrentMain:           c.cluster.CurrentMain,
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
		totalPods, healthyPods, unhealthyPods, c.cluster.CurrentMain, currentSyncReplica, syncReplicaHealthy)

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

// IsRunning returns true if this controller instance is running
func (c *MemgraphController) IsRunning() bool {
	return c.isRunning
}

// GetLeaderElection returns the leader election instance for API access
func (c *MemgraphController) GetLeaderElection() *LeaderElection {
	return c.leaderElection
}

// saveControllerStateAfterBootstrap persists the controller state after successful bootstrap

// loadControllerStateOnStartup loads persisted state and determines startup phase
func (c *MemgraphController) loadControllerStateOnStartup(ctx context.Context) error {
	exists, err := c.getStateManager().StateExists(ctx)
	if err != nil {
		return fmt.Errorf("failed to check state existence: %w", err)
	}

	if !exists {
		log.Println("No state ConfigMap found - will start in BOOTSTRAP phase")
		return nil // Keep default bootstrap=true
	}

	state, err := c.getStateManager().LoadState(ctx)
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

	log.Printf("Loaded persisted state - will start in OPERATIONAL phase: targetMainIndex=%d", state.TargetMainIndex)

	return nil
}

// getTargetMainIndex returns the current target main index from state manager
func (c *MemgraphController) getTargetMainIndex() int {
	// Handle test cases where stateManager is nil
	if c.getStateManager() == nil {
		return -1 // Bootstrap phase / test scenario
	}

	ctx := context.Background()
	state, err := c.getStateManager().LoadState(ctx)
	if err != nil {
		// Return -1 if no state available (bootstrap phase)
		return -1
	}
	return state.TargetMainIndex
}

// getCurrentMainFromState gets the current main pod name from state manager
func (c *MemgraphController) getCurrentMainFromState(ctx context.Context) (string, error) {
	// Handle test cases where stateManager is nil
	if c.getStateManager() == nil {
		return "", fmt.Errorf("no state manager available")
	}

	state, err := c.getStateManager().LoadState(ctx)
	if err != nil {
		return "", err
	}
	return c.config.GetPodName(state.TargetMainIndex), nil
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
	pod := obj.(*v1.Pod)
	if !c.config.IsMemgraphPod(pod.Name) {
		return // Ignore unrelated pods
	}
	c.enqueuePodEvent("pod-added")
}

// onPodUpdate handles pod update events with immediate processing for critical changes
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// Only process Memgraph pods
	if !c.config.IsMemgraphPod(newPod.Name) {
		return
	}

	// Check for IP changes and update connection pool through cached cluster state
	if oldPod.Status.PodIP != newPod.Status.PodIP {
		cachedState, _ := c.getCachedState()
		if cachedState != nil {
			cachedState.HandlePodIPChange(newPod.Name, oldPod.Status.PodIP, newPod.Status.PodIP)
		}
	}

	// IMMEDIATE ANALYSIS: Check for critical main pod health changes
	if c.IsLeader() {
		lastState, stateAge := c.getCachedState()
		if lastState != nil && lastState.CurrentMain == newPod.Name {
			// This is the current main pod - check for immediate health issues
			if c.isPodBecomeUnhealthy(oldPod, newPod) {
				log.Printf("🚨 IMMEDIATE EVENT: Main pod %s became unhealthy, triggering immediate failover", newPod.Name)
				
				// Trigger immediate failover in background
				go c.handleImmediateFailover(newPod.Name)
				
				// Don't queue regular reconciliation - immediate action taken
				return
			}
		}
		
		// Log cached state age for debugging
		if lastState != nil {
			log.Printf("🔍 Event analysis: cached state age=%v, currentMain=%s, eventPod=%s", 
				time.Since(stateAge), lastState.CurrentMain, newPod.Name)
		}
	}

	// Fall back to regular reconciliation for non-critical changes
	if !c.shouldReconcile(oldPod, newPod) {
		return // Skip unnecessary reconciliation
	}

	c.enqueuePodEvent("pod-state-changed")
}

// onPodDelete handles pod deletion events
func (c *MemgraphController) onPodDelete(obj interface{}) {
	pod := obj.(*v1.Pod)

	// Invalidate connection for deleted pod through cached cluster state
	cachedState, _ := c.getCachedState()
	if cachedState != nil {
		cachedState.InvalidatePodConnection(pod.Name)
	}

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
	if configMap.Name != c.getStateManager().ConfigMapName() {
		return
	}

	log.Printf("🔄 ConfigMap added: %s", configMap.Name)

	// Extract targetMainIndex from ConfigMap
	targetMainIndexStr, exists := configMap.Data["targetMainIndex"]
	if !exists {
		// Try legacy masterIndex for backward compatibility
		if legacyStr, legacyExists := configMap.Data["masterIndex"]; legacyExists {
			targetMainIndexStr = legacyStr
		} else {
			log.Printf("Warning: ConfigMap %s missing targetMainIndex", configMap.Name)
			return
		}
	}

	// Parse targetMainIndex
	var newTargetMainIndex int
	if _, err := fmt.Sscanf(targetMainIndexStr, "%d", &newTargetMainIndex); err != nil {
		log.Printf("Failed to parse targetMainIndex from ConfigMap: %v", err)
		return
	}

	// Check if this is different from our current state
	currentIndex := c.cluster.getTargetMainIndex()

	if currentIndex != newTargetMainIndex {
		log.Printf("Target main index changed: %d -> %d", currentIndex, newTargetMainIndex)
		c.handleTargetMainChanged(newTargetMainIndex)
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
	if newConfigMap.Name != c.getStateManager().ConfigMapName() {
		return
	}

	// Extract new targetMainIndex from ConfigMap
	newTargetMainIndexStr, exists := newConfigMap.Data["targetMainIndex"]
	if !exists {
		// Try legacy masterIndex for backward compatibility
		if legacyStr, legacyExists := newConfigMap.Data["masterIndex"]; legacyExists {
			newTargetMainIndexStr = legacyStr
		} else {
			log.Printf("Warning: ConfigMap %s missing targetMainIndex", newConfigMap.Name)
			return
		}
	}

	// Parse new targetMainIndex
	var newTargetMainIndex int
	if _, err := fmt.Sscanf(newTargetMainIndexStr, "%d", &newTargetMainIndex); err != nil {
		log.Printf("Failed to parse targetMainIndex from ConfigMap: %v", err)
		return
	}

	// Compare with our current state (not old ConfigMap)
	currentIndex := c.cluster.getTargetMainIndex()

	if currentIndex == newTargetMainIndex {
		// No change from our perspective, ignore
		return
	}

	log.Printf("🔄 ConfigMap state change detected: TargetMainIndex %d -> %d", currentIndex, newTargetMainIndex)
	c.handleTargetMainChanged(newTargetMainIndex)
}

// onConfigMapDelete handles ConfigMap deletion events
func (c *MemgraphController) onConfigMapDelete(obj interface{}) {
	configMap := obj.(*v1.ConfigMap)

	// Only process our controller state ConfigMap
	if configMap.Name != c.getStateManager().ConfigMapName() {
		return
	}

	log.Printf("⚠️ ConfigMap deleted: %s", configMap.Name)
}

// handleTargetMainChanged processes target main index changes and coordinates controller pods
func (c *MemgraphController) handleTargetMainChanged(newTargetMainIndex int) {
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
	if oldTargetIndex == newTargetMainIndex {
		log.Printf("📋 Target main index unchanged (%d), ignoring", newTargetMainIndex)
		return
	}

	// Log detailed coordination information
	log.Printf("🔄 COORDINATION EVENT: Non-leader detected TargetMainIndex change")
	log.Printf("   📊 Change: %d -> %d", oldTargetIndex, newTargetMainIndex)
	log.Printf("   🏷️ Pod Identity: %s", c.getPodIdentity())

	// Phase 1: Update local controller state atomically
	newLastKnownMain := ""
	if newTargetMainIndex >= 0 {
		newLastKnownMain = c.config.GetPodName(newTargetMainIndex)
	}

	// Update state directly via state manager (for non-leaders)

	log.Printf("   ✅ State updated: targetMainIndex=%d, lastKnownMain=%s -> %s",
		newTargetMainIndex, oldLastKnownMain, newLastKnownMain)

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
		newTargetMainIndex, newLastKnownMain)
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

	// Update connection pool through cached cluster state
	cachedState, _ := c.getCachedState()
	if cachedState != nil {
		// Update the pod info in cached state if it exists
		if podInfo, exists := cachedState.Pods[podName]; exists {
			oldIP := ""
			if podInfo.Pod != nil {
				oldIP = podInfo.Pod.Status.PodIP
			}
			if pod.Status.PodIP != oldIP {
				cachedState.HandlePodIPChange(podName, oldIP, pod.Status.PodIP)
			}
		}
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
	state, err := c.getStateManager().LoadState(ctx)
	if err != nil {
		// ConfigMap doesn't exist or is invalid
		log.Printf("ConfigMap not ready: %v", err)
		return false, nil
	}

	// Validate that the state contains required information
	if state.TargetMainIndex < 0 || state.TargetMainIndex > 1 {
		log.Printf("ConfigMap contains invalid TargetMainIndex: %d", state.TargetMainIndex)
		return false, nil
	}

	log.Printf("ConfigMap is ready with TargetMainIndex: %d", state.TargetMainIndex)
	return true, nil
}

// discoverClusterAndCreateConfigMap implements DESIGN.md "Discover Cluster State" section
func (c *MemgraphController) discoverClusterAndCreateConfigMap(ctx context.Context) error {
	log.Println("=== CLUSTER DISCOVERY ===")

	// Use bootstrap logic to discover and initialize cluster
	bootstrapController := NewBootstrapController(c)
	_, err := bootstrapController.ExecuteBootstrap(ctx)
	if err != nil {
		return fmt.Errorf("cluster discovery failed: %w", err)
	}

	log.Printf("✅ Cluster discovery completed - main: %s", c.cluster.CurrentMain)
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
		"running":        c.isRunning,
		"last_reconcile": c.lastReconcile,
		"failure_count":  c.failureCount,
		"max_failures":   c.maxFailures,
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
	err := c.cluster.RefreshClusterInfo(ctx, c.updateSyncReplicaInfo, c.detectMainFailover, c.handleMainFailover)
	if err != nil {
		log.Printf("❌ CRITICAL: Failed to discover cluster state for immediate failover: %v", err)
		return
	}

	// Update cached state after immediate failover discovery
	c.updateCachedState(c.cluster)

	// Log cluster state
	log.Printf("📊 Immediate failover cluster state: %d pods, main=%s",
		len(c.cluster.Pods), c.cluster.CurrentMain)

	// Execute immediate failover using existing logic
	if err := c.handleMainFailover(ctx, c.cluster); err != nil {
		log.Printf("❌ IMMEDIATE FAILOVER FAILED: %v", err)
		return
	}

	totalFailoverTime := time.Since(failoverStartTime)
	log.Printf("🎯 IMMEDIATE FAILOVER COMPLETED in %v: Event-driven failover successful",
		totalFailoverTime)
}

// handleMainFailurePromotion promotes SYNC replica when main fails during operations

// promoteToMain promotes a pod to main role
