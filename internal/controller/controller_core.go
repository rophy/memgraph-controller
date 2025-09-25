package controller

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"memgraph-controller/internal/common"
	"memgraph-controller/internal/gateway"
	"memgraph-controller/internal/httpapi"
	"memgraph-controller/internal/metrics"
)

type MemgraphController struct {
	ctx               context.Context // Base context for the controller
	clientset         kubernetes.Interface
	config            *common.Config
	memgraphClient    *MemgraphClient // Singleton instance - all components use this
	httpServer        *httpapi.HTTPServer
	gatewayServer     *gateway.Server // Read/write gateway (port 7687)
	readGatewayServer *gateway.Server // Read-only gateway (port 7688)

	// Leader election
	leaderElection *LeaderElection

	// State management
	targetMainIndex atomic.Int32
	configMapName   string
	targetMutex     sync.RWMutex

	// Event-driven reconciliation
	reconcileQueue      *ReconcileQueue
	failoverCheckQueue  *FailoverCheckQueue
	failoverCheckNeeded atomic.Bool

	// Shared mutex for reconciliation and failover operations
	// This prevents race conditions between these operations
	operationMu sync.Mutex

	// Cluster operations
	cluster *MemgraphCluster

	// Removed: In-memory cluster state for immediate event processing (obsolete)

	// Controller loop state
	isRunning   bool
	mu          sync.RWMutex
	maxFailures int

	// Event-driven reconciliation
	podInformer     cache.SharedInformer
	informerFactory informers.SharedInformerFactory
	stopCh          chan struct{}

	// Reconciliation metrics (deprecated - kept for compatibility)
	metrics *ReconciliationMetrics

	// Prometheus metrics
	promMetrics *metrics.Metrics

	// Health prober for blackbox monitoring
	healthProber *HealthProber
}

func NewMemgraphController(ctx context.Context, clientset kubernetes.Interface, config *common.Config) *MemgraphController {
	logger := common.GetLoggerFromContext(ctx)
	// Create single MemgraphClient instance with its own connection pool
	// All components will use this singleton instance
	memgraphClient := NewMemgraphClient(config)

	controller := &MemgraphController{
		ctx:                 ctx,
		clientset:           clientset,
		config:              config,
		memgraphClient:      memgraphClient, // Singleton instance
		failoverCheckNeeded: atomic.Bool{},
	}

	controller.failoverCheckNeeded.Store(false)

	// Initialize HTTP server
	controller.httpServer = httpapi.NewHTTPServer(controller, config)

	// Initialize read/write gateway server (port 7687)
	gatewayConfig := gateway.LoadGatewayConfig()
	gatewayConfig.BindAddress = config.GatewayBindAddress
	controller.gatewayServer = gateway.NewServer("main-gw", gatewayConfig)

	// Initialize read-only gateway server (port 7688)
	// Check if read-only gateway is enabled via environment variable
	if os.Getenv("ENABLE_READ_GATEWAY") == "true" {
		readGatewayConfig := gateway.LoadGatewayConfig()
		// Change port to 7688 for read-only traffic
		readGatewayConfig.BindAddress = "0.0.0.0:7688"
		controller.readGatewayServer = gateway.NewServer("read-gw", readGatewayConfig)

		// Set up upstream failure callback for read gateway
		controller.readGatewayServer.SetUpstreamFailureCallback(controller.handleReadGatewayUpstreamFailure)
	}

	// Initialize state management with release-based ConfigMap name
	configMapName := "memgraph-controller-state"
	releaseName := os.Getenv("RELEASE_NAME")
	if releaseName != "" {
		configMapName = fmt.Sprintf("%s-state", releaseName)
	}
	if len(configMapName) > 63 {
		configMapName = configMapName[:63]
	}
	controller.configMapName = configMapName

	controller.targetMainIndex.Store(-1) // -1 indicates not yet loaded from ConfigMap
	// Initialize controller state
	controller.maxFailures = 5
	controller.stopCh = make(chan struct{})

	// Initialize reconciliation metrics
	controller.metrics = &ReconciliationMetrics{}

	// Initialize cluster operations (only if we have a clientset for informers)
	if clientset != nil {
		// Note: This will be properly initialized after StartInformers() is called
		// For now, we'll defer cluster initialization
		controller.cluster = nil
	} else {
		controller.cluster = nil
	}

	// Initialize health prober with default configuration
	proberConfig := DefaultProberConfig()
	controller.healthProber = NewHealthProber(controller, proberConfig)

	// Initialize leader election
	leaderElection, err := NewLeaderElection(clientset, config)
	if err != nil {
		logger.Error("failed to initialize leader election", "error", err)
		return nil
	}
	controller.leaderElection = leaderElection

	return controller
}

// Initialize starts all controller components (informers, servers, leader election)
// This should be called after NewMemgraphController but before Run
func (c *MemgraphController) Initialize(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
	// Initialize event-driven reconciliation queue
	c.reconcileQueue = c.newReconcileQueue(ctx)
	c.failoverCheckQueue = c.newFailoverCheckQueue(ctx)

	// STEP 1: Start Kubernetes informers
	if err := c.StartInformers(ctx); err != nil {
		return fmt.Errorf("failed to start informers: %w", err)
	}

	// STEP 2: Start HTTP server for status API (always running, not leader-dependent)
	c.httpServer.Start(ctx)

	// STEP 3: Start gateway server
	if err := c.StartGatewayServer(ctx); err != nil {
		return fmt.Errorf("failed to start gateway server: %w", err)
	}

	// STEP 4: Start leader election (in background goroutine)
	c.startLeaderElection(ctx)

	logger.Info("✅ All controller components initialized successfully")
	return nil
}

// Shutdown stops all controller components gracefully
func (c *MemgraphController) Shutdown(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
	logger.Info("Shutting down all controller components...")

	// Stop health prober
	if c.healthProber != nil {
		c.healthProber.Stop()
		logger.Info("Health prober stopped")
	}

	// Stop gateway server
	if err := c.StopGatewayServer(ctx); err != nil {
		logger.Info("Gateway server shutdown error", "error", err)
	} else {
		logger.Info("Gateway server stopped successfully")
	}

	// Stop informers
	c.StopInformers()
	logger.Info("Informers stopped successfully")

	logger.Info("✅ All controller components shut down successfully")
	return nil
}

// GetTargetMainIndex returns the target main index from ConfigMap or error if not available
func (c *MemgraphController) GetTargetMainIndex(ctx context.Context) (int32, error) {
	index := c.targetMainIndex.Load()
	if index != -1 {
		return index, nil
	}

	// Need to load from ConfigMap
	c.targetMutex.Lock()
	defer c.targetMutex.Unlock()

	logger := common.GetLoggerFromContext(ctx)
	logger.Debug("GetTargetMainIndex: target main index not found in memory, loading from ConfigMap")

	// Load state from ConfigMap
	configMap, err := c.clientset.CoreV1().ConfigMaps(c.config.Namespace).Get(ctx, c.configMapName, metav1.GetOptions{})
	if err != nil {
		return -1, fmt.Errorf("ConfigMap not available: %w", err)
	}

	targetMainIndexStr, exists := configMap.Data["targetMainIndex"]
	if !exists {
		return -1, fmt.Errorf("targetMainIndex not found in ConfigMap")
	}

	var targetMainIndex int32
	if _, err := fmt.Sscanf(targetMainIndexStr, "%d", &targetMainIndex); err != nil {
		return -1, fmt.Errorf("invalid targetMainIndex format: %w", err)
	}

	c.targetMainIndex.Store(targetMainIndex)

	logger.Info("GetTargetMainIndex: loaded target main index from ConfigMap", "index", targetMainIndex)
	return targetMainIndex, nil
}

// SetTargetMainIndex updates both in-memory target and ConfigMap
func (c *MemgraphController) SetTargetMainIndex(ctx context.Context, index int32) error {
	c.targetMutex.Lock()
	defer c.targetMutex.Unlock()

	logger := common.GetLoggerFromContext(ctx)
	logger.Debug("SetTargetMainIndex: updating target main index in ConfigMap", "index", index)

	// Get owner reference to the controller pod for proper cleanup
	ownerRef, err := c.getControllerOwnerReference(ctx)
	if err != nil {
		logger.Warn("Failed to get controller owner reference - ConfigMap will not be cleaned up automatically", "error", err)
	}

	// Update ConfigMap first
	configMapData := map[string]string{
		"targetMainIndex": fmt.Sprintf("%d", index),
	}

	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.configMapName,
			Namespace: c.config.Namespace,
		},
		Data: configMapData,
	}

	// Set owner reference if available
	if ownerRef != nil {
		configMap.ObjectMeta.OwnerReferences = []metav1.OwnerReference{*ownerRef}
	}

	_, err = c.clientset.CoreV1().ConfigMaps(c.config.Namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	if err != nil {
		// Try to create if it doesn't exist
		if errors.IsNotFound(err) {
			_, err = c.clientset.CoreV1().ConfigMaps(c.config.Namespace).Create(ctx, configMap, metav1.CreateOptions{})
		}
		if err != nil {
			return fmt.Errorf("failed to update ConfigMap: %w", err)
		}
	}

	// Check if main node changed
	oldIndex := c.targetMainIndex.Load()
	if oldIndex != index && oldIndex != -1 && c.promMetrics != nil {
		c.promMetrics.RecordMainChange()
	}

	// Update in-memory value
	c.targetMainIndex.Store(index)
	logger.Info("SetTargetMainIndex: updated target main index in ConfigMap", "index", index)
	return nil
}

// getTargetMainNode returns the node that should be main based on TargetMainIndex
func (c *MemgraphController) getTargetMainNode(ctx context.Context) (*MemgraphNode, error) {
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get target main index: %w", err)
	}
	podName := c.config.GetPodName(targetMainIndex)
	node, exists := c.cluster.MemgraphNodes[podName]
	if !exists {
		return nil, fmt.Errorf("target main pod %s not found in cluster state", podName)
	}
	return node, nil
}

// getTargetSyncReplicaNode returns the node that should be sync replica (complement of main)
func (c *MemgraphController) getTargetSyncReplicaNode(ctx context.Context) (*MemgraphNode, error) {
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get target main index: %w", err)
	}
	var targetSyncIndex int32
	if targetMainIndex == 0 {
		targetSyncIndex = 1
	} else {
		targetSyncIndex = 0
	}
	podName := c.config.GetPodName(targetSyncIndex)
	node, exists := c.cluster.MemgraphNodes[podName]
	if !exists {
		return nil, fmt.Errorf("target sync replica pod %s not found in cluster state", podName)
	}
	return node, nil
}

// IsRunning returns whether the controller is currently running
func (c *MemgraphController) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isRunning
}

// GetLeaderElection returns the leader election instance for testing
func (c *MemgraphController) GetLeaderElection() httpapi.LeaderElectionInterface {
	return c.leaderElection
}

// TestConnection tests basic connectivity to Kubernetes API
func (c *MemgraphController) TestConnection() error {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	ctx, logger := common.NewLoggerContext(ctx)
	defer cancel()

	_, err := c.clientset.CoreV1().Pods(c.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", c.config.AppName),
		Limit:         1,
	})

	if err != nil {
		return fmt.Errorf("failed to connect to Kubernetes API: %w", err)
	}

	logger.Info("Successfully connected to Kubernetes API", "app_name", c.config.AppName, "namespace", c.config.Namespace)
	return nil
}

// GetCurrentMainNode returns the current main MemgraphNode for the gateway
func (c *MemgraphController) GetCurrentMainNode(ctx context.Context) (*MemgraphNode, error) {
	// During failover, the target main index may point to a pod that isn't actually main yet
	// We need to find the pod that ACTUALLY has the main role in Memgraph

	// First, ensure we have current cluster state
	if c.cluster == nil || len(c.cluster.MemgraphNodes) == 0 {
		// Fallback to target-based approach if no cluster state
		targetMainIndex, err := c.GetTargetMainIndex(ctx)
		if err != nil {
			return nil, fmt.Errorf("no cluster state and failed to get target main index: %w", err)
		}
		podName := c.config.GetPodName(targetMainIndex)
		pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get pod %s: %w", podName, err)
		}
		if pod.Status.PodIP == "" {
			return nil, fmt.Errorf("pod %s has no IP address assigned", podName)
		}
		mainNode := NewMemgraphNode(pod, c.memgraphClient)
		return mainNode, nil
	}

	// Look for the pod that actually has the main role
	for _, node := range c.cluster.MemgraphNodes {
		if role, _ := node.GetReplicationRole(ctx, false); role == "MAIN" {
			// Check if pod exists in cache and has IP
			pod, err := c.getPodFromCache(node.GetName())
			if err == nil && pod != nil && pod.Status.PodIP != "" {
				// Found a pod that is actually in main role
				return node, nil
			}
		}
	}

	// If no pod has main role yet (during failover transition), fall back to target
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("no main node found and failed to get target main index: %w", err)
	}
	podName := c.config.GetPodName(targetMainIndex)

	// Check if the target pod exists in cluster state
	if node, exists := c.cluster.MemgraphNodes[podName]; exists {
		// Check if pod exists in cache
		_, err := c.getPodFromCache(node.GetName())
		if err == nil {
			return node, nil
		}
	}

	// Last resort: fetch the pod directly
	pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s: %w", podName, err)
	}
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("pod %s has no IP address assigned", podName)
	}
	mainNode := NewMemgraphNode(pod, c.memgraphClient)
	return mainNode, nil
}

// StartGatewayServer starts the gateway servers (both read/write and read-only if enabled)
func (c *MemgraphController) StartGatewayServer(ctx context.Context) error {
	ctx, logger := common.WithAttr(ctx, "thread", "gatewayServices")
	logger.Info("Starting gateway servers...")
	// Start read/write gateway
	if err := c.gatewayServer.Start(ctx); err != nil {
		return fmt.Errorf("failed to start read/write gateway: %w", err)
	}

	// Start read-only gateway if enabled
	if c.readGatewayServer != nil {
		if err := c.readGatewayServer.Start(ctx); err != nil {
			return fmt.Errorf("failed to start read-only gateway: %w", err)
		}
		logger.Info("Read-only gateway server started on port 7688")
	}
	logger.Info("Gateway servers started successfully")
	return nil
}

// StopGatewayServer stops the gateway server (no-op - process termination handles cleanup)
func (c *MemgraphController) StopGatewayServer(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
	logger.Info("Gateway: No explicit shutdown needed - process termination handles cleanup")
	return nil
}

// selectBestReplica selects the best available replica for read traffic
func (c *MemgraphController) selectBestReplica(ctx context.Context) (*MemgraphNode, error) {
	// Get the target main node to exclude it
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get target main index: %w", err)
	}
	targetMainPodName := fmt.Sprintf("%s-%d", c.config.StatefulSetName, targetMainIndex)

	// Filter healthy replica nodes from the existing cluster state
	var replicas []*MemgraphNode
	for podName, node := range c.cluster.MemgraphNodes {
		// Skip if this is the main pod
		if podName == targetMainPodName {
			continue
		}

		// Check if pod is ready using cached pod info
		pod, err := c.getPodFromCache(podName)
		if err != nil || !isPodReady(pod) {
			continue
		}

		// Check if it's actually a replica
		role, err := node.GetReplicationRole(ctx, false)
		if err != nil || role != "replica" {
			continue
		}

		replicas = append(replicas, node)
	}

	// Order replica nodes by name descending
	sort.Slice(replicas, func(i, j int) bool {
		return replicas[i].GetName() > replicas[j].GetName()
	})

	// The best replica is the LAST one in a StatefulSet
	// Such that SYNC replica is the least preferred one.
	if len(replicas) > 0 {
		return replicas[0], nil
	}

	return nil, fmt.Errorf("no healthy replicas available")
}

// updateReadGatewayUpstream updates the read-only gateway's upstream address
func (c *MemgraphController) updateReadGatewayUpstream(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
	// Skip if read gateway is not enabled
	if c.readGatewayServer == nil {
		return
	}

	// Try to find a healthy replica
	replica, err := c.selectBestReplica(ctx)
	if err != nil {
		// Fall back to main if no replicas available
		logger.Warn("No healthy replicas available for read gateway, falling back to main", "error", err)

		// Use main pod as fallback
		targetMainNode, err := c.getTargetMainNode(ctx)
		if err != nil {
			logger.Error("Failed to get main node for read gateway fallback", "error", err)
			c.readGatewayServer.SetUpstreamAddress(ctx, "")
			return
		}

		boltAddress, err := targetMainNode.GetBoltAddress()
		if err != nil {
			logger.Error("Failed to get bolt address for main node", "error", err)
			c.readGatewayServer.SetUpstreamAddress(ctx, "")
			return
		}

		c.readGatewayServer.SetUpstreamAddress(ctx, boltAddress)
		logger.Info("Read gateway using main as fallback", "address", boltAddress)
		return
	}

	// Set replica address for read gateway
	boltAddress, err := replica.GetBoltAddress()
	if err != nil {
		logger.Error("Failed to get bolt address for replica", "error", err)
		c.readGatewayServer.SetUpstreamAddress(ctx, "")
		return
	}

	c.readGatewayServer.SetUpstreamAddress(ctx, boltAddress)
	logger.Info("Read gateway updated with replica", "replica", replica.GetName(), "address", boltAddress)
}

// stop performs cleanup when the controller stops
func (c *MemgraphController) stop() {
	logger := common.GetLogger()
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.isRunning {
		return // Already stopped
	}

	logger.Info("Stopping Memgraph Controller...")
	c.isRunning = false

	// Stop informers
	if c.stopCh != nil {
		close(c.stopCh)
	}

	// Stop reconcile queue
	c.stopReconcileQueue()

	// Stop failover check queue
	c.stopFailoverCheckQueue()

	// Gateway cleanup handled by process termination
	logger.Info("Gateway: Cleanup will be handled by process termination")

	logger.Info("Memgraph Controller stopped")
}

// SetPrometheusMetrics sets the Prometheus metrics instance
func (c *MemgraphController) SetPrometheusMetrics(m *metrics.Metrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.promMetrics = m

	// Also set metrics on the gateway servers if they exist
	if c.gatewayServer != nil {
		c.gatewayServer.SetPrometheusMetrics(m)
	}
	if c.readGatewayServer != nil {
		c.readGatewayServer.SetPrometheusMetrics(m)
	}
}

// GetControllerStatus returns the current status of the controller
func (c *MemgraphController) GetControllerStatus() map[string]interface{} {
	return map[string]interface{}{
		"is_running": c.IsRunning(),
		"metrics":    c.GetReconciliationMetrics(),
	}
}

// getPodFromCache retrieves a pod from the informer cache instead of making API calls
func (c *MemgraphController) getPodFromCache(podName string) (*v1.Pod, error) {
	key := fmt.Sprintf("%s/%s", c.config.Namespace, podName)
	obj, exists, err := c.podInformer.GetStore().GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s from cache: %w", podName, err)
	}
	if !exists {
		return nil, fmt.Errorf("pod %s not found in cache", podName)
	}
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return nil, fmt.Errorf("cached object for %s is not a Pod", podName)
	}
	return pod, nil
}

// getControllerOwnerReference creates an owner reference pointing to the controller deployment
// This ensures ConfigMaps are cleaned up when the controller is uninstalled
func (c *MemgraphController) getControllerOwnerReference(ctx context.Context) (*metav1.OwnerReference, error) {
	// Get current pod name from environment (set by Kubernetes via fieldRef)
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		return nil, fmt.Errorf("POD_NAME environment variable not set")
	}

	// Get the controller pod to find its owner (the Deployment)
	pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get controller pod %s: %w", podName, err)
	}

	// Find the Deployment owner reference
	for _, ownerRef := range pod.OwnerReferences {
		if ownerRef.Kind == "ReplicaSet" && ownerRef.APIVersion == "apps/v1" {
			// Get the ReplicaSet to find its Deployment owner
			rs, err := c.clientset.AppsV1().ReplicaSets(c.config.Namespace).Get(ctx, ownerRef.Name, metav1.GetOptions{})
			if err != nil {
				continue // Try next owner reference
			}

			// Find the Deployment owner of this ReplicaSet
			for _, rsOwnerRef := range rs.OwnerReferences {
				if rsOwnerRef.Kind == "Deployment" && rsOwnerRef.APIVersion == "apps/v1" {
					return &metav1.OwnerReference{
						APIVersion: rsOwnerRef.APIVersion,
						Kind:       rsOwnerRef.Kind,
						Name:       rsOwnerRef.Name,
						UID:        rsOwnerRef.UID,
						Controller: &[]bool{true}[0], // Create a pointer to true
					}, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("could not find Deployment owner reference for pod %s", podName)
}

// ResetAllConnections closes all existing Neo4j connections and clears the connection pool
func (c *MemgraphController) ResetAllConnections(ctx context.Context) (int, error) {
	logger := common.GetLoggerFromContext(ctx)
	logger.Info("Admin API: Resetting all Memgraph connections")

	totalConnections := 0

	// Reset the singleton MemgraphClient's connection pool
	if c.memgraphClient != nil && c.memgraphClient.connectionPool != nil {
		c.memgraphClient.connectionPool.mutex.RLock()
		controllerConnections := len(c.memgraphClient.connectionPool.drivers)
		c.memgraphClient.connectionPool.mutex.RUnlock()

		c.memgraphClient.connectionPool.Close(ctx)
		totalConnections += controllerConnections
		logger.Info("Admin API: Reset singleton MemgraphClient connection pool", "count", controllerConnections)
	} else {
		logger.Warn("Admin API: No MemgraphClient connection pool to reset")
	}

	// Reset gateway connections (both read/write and read-only)
	if c.gatewayServer != nil {
		c.gatewayServer.DisconnectAll(ctx)
		logger.Info("Admin API: Reset read/write gateway connections")
	}
	if c.readGatewayServer != nil {
		c.readGatewayServer.DisconnectAll(ctx)
		logger.Info("Admin API: Reset read-only gateway connections")
	}
	if c.gatewayServer == nil && c.readGatewayServer == nil {
		logger.Warn("Admin API: No gateway server to reset")
	}

	logger.Info("Admin API: Successfully reset all connections", "total_count", totalConnections)
	return totalConnections, nil
}

// ClearGatewayUpstreams clears the upstream addresses for both gateways (for preStop hooks)
func (c *MemgraphController) ClearGatewayUpstreams(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
	logger.Info("Admin API: PreStop hook - clearing gateway upstreams")

	// Clear main gateway upstream
	if c.gatewayServer != nil {
		c.gatewayServer.SetUpstreamAddress(ctx, "")
		logger.Info("Admin API: Cleared main gateway upstream")
	}

	// Clear read gateway upstream
	if c.readGatewayServer != nil {
		c.readGatewayServer.SetUpstreamAddress(ctx, "")
		logger.Info("Admin API: Cleared read gateway upstream")
	}

	if c.gatewayServer == nil && c.readGatewayServer == nil {
		logger.Warn("Admin API: No gateway servers found to clear upstreams")
		return fmt.Errorf("no gateway servers available")
	}

	logger.Info("Admin API: Successfully cleared all gateway upstreams")
	return nil
}

// handleReadGatewayUpstreamFailure handles upstream failures reported by the read gateway
func (c *MemgraphController) handleReadGatewayUpstreamFailure(ctx context.Context, upstreamAddress string, err error) {
	logger := common.GetLoggerFromContext(ctx)
	logger.Warn("Read gateway reported upstream failure, triggering upstream switch",
		"upstream_address", upstreamAddress,
		"error", err.Error(),
		"action", "immediate_upstream_switch")

	// Immediately switch to a different replica
	c.updateReadGatewayUpstream(ctx)

	newUpstream := ""
	if c.readGatewayServer != nil {
		newUpstream = c.readGatewayServer.GetUpstreamAddress()
	}

	logger.Info("Read gateway upstream switch completed after connection failure",
		"failed_upstream", upstreamAddress,
		"new_upstream", newUpstream)
}

// IsLeader returns whether the controller is the current leader
func (c *MemgraphController) IsLeader() bool {
	return c.leaderElection.IsLeader()
}

// GetClusterStatus returns comprehensive cluster status for the HTTP API
func (c *MemgraphController) GetClusterStatus(ctx context.Context) (*httpapi.StatusResponse, error) {
	logger := common.GetLoggerFromContext(ctx)
	clusterState := c.cluster
	if clusterState == nil {
		return nil, fmt.Errorf("no cluster state available")
	}

	// Get current main from controller's target main index
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	currentMain := ""
	if err == nil {
		currentMain = c.config.GetPodName(targetMainIndex)
	}

	// Build cluster status summary
	statusResponse := httpapi.ClusterStatus{
		CurrentMain:        currentMain,
		CurrentSyncReplica: "", // Will be determined below
		TotalPods:          len(clusterState.MemgraphNodes),
	}

	// Count healthy vs unhealthy pods and find sync replica
	healthyCount := 0
	syncReplicaHealthy := false
	for podName := range clusterState.MemgraphNodes {
		if cachedPod, err := c.getPodFromCache(podName); err == nil && isPodReady(cachedPod) {
			healthyCount++
			// Check if this is the sync replica
			if targetSyncReplica, err := c.getTargetSyncReplicaNode(ctx); err == nil && podName == targetSyncReplica.GetName() {
				syncReplicaHealthy = true
				statusResponse.CurrentSyncReplica = podName
			}
		}
	}
	statusResponse.HealthyPods = healthyCount
	statusResponse.UnhealthyPods = statusResponse.TotalPods - healthyCount
	statusResponse.SyncReplicaHealthy = syncReplicaHealthy

	// Update Prometheus cluster state metrics
	if c.promMetrics != nil {
		clusterHealthy := healthyCount == statusResponse.TotalPods && currentMain != ""
		c.promMetrics.UpdateClusterState(clusterHealthy, statusResponse.TotalPods, healthyCount, syncReplicaHealthy)
	}

	// Convert pods to API format
	var pods []httpapi.PodStatus
	for _, node := range clusterState.MemgraphNodes {
		pod, err := c.getPodFromCache(node.GetName())
		healthy := err == nil && isPodReady(pod)
		podStatus := convertMemgraphNodeToStatus(node, healthy, pod)
		pods = append(pods, podStatus)
	}

	// Get replica registrations from the main node
	var replicaRegistrations []httpapi.ReplicaRegistration
	if targetMainIndex, err := c.GetTargetMainIndex(ctx); err == nil {
		targetMainPodName := c.config.GetPodName(targetMainIndex)
		if mainNode, exists := clusterState.MemgraphNodes[targetMainPodName]; exists {
			if replicas, err := mainNode.GetReplicas(ctx, false); err == nil {
				for _, replica := range replicas {
					replicaRegistrations = append(replicaRegistrations, httpapi.ReplicaRegistration{
						Name:      replica.Name,
						PodName:   replica.GetPodName(),
						Address:   replica.SocketAddress,
						SyncMode:  replica.SyncMode,
						IsHealthy: replica.IsHealthy(),
					})
				}
			}
		}
	}

	// Add leader status and reconciliation metrics to cluster status
	statusResponse.ReconciliationMetrics = c.GetReconciliationMetrics()
	statusResponse.ReplicaRegistrations = replicaRegistrations

	// Add read gateway status if enabled
	if c.readGatewayServer != nil {
		statusResponse.ReadGatewayStatus = c.getReadGatewayStatus(ctx)
	}

	response := &httpapi.StatusResponse{
		Timestamp:    time.Now(),
		ClusterState: statusResponse,
		Pods:         pods,
	}

	logger.Info("Generated cluster status",
		"pod_count", len(pods), "current_main", currentMain, "healthy_pods", healthyCount, "total_pods", statusResponse.TotalPods)

	return response, nil
}

// getReadGatewayStatus returns the current status of the read gateway
func (c *MemgraphController) getReadGatewayStatus(ctx context.Context) *httpapi.ReadGatewayStatus {
	if c.readGatewayServer == nil {
		return nil
	}

	// Get gateway statistics
	gatewayStats := c.readGatewayServer.GetStats()
	currentUpstream := c.readGatewayServer.GetUpstreamAddress()

	// Get health check statistics from health prober
	healthCheckStats := httpapi.HealthCheckStats{
		ConsecutiveFailures: 0,
		LastHealthStatus:    true,
	}

	if c.healthProber != nil {
		// Get read gateway health stats from prober
		c.healthProber.mu.RLock()
		healthCheckStats.ConsecutiveFailures = c.healthProber.readGatewayConsecutiveFailures
		healthCheckStats.LastHealthStatus = c.healthProber.lastReadGatewayHealthStatus
		c.healthProber.mu.RUnlock()
	}

	// Determine upstream pod name from address
	upstreamPodName := ""
	upstreamHealthy := false
	if currentUpstream != "" {
		upstreamPodName = c.getUpstreamPodName(currentUpstream)
		upstreamHealthy = healthCheckStats.LastHealthStatus && healthCheckStats.ConsecutiveFailures == 0
	}

	return &httpapi.ReadGatewayStatus{
		Enabled:         true,
		CurrentUpstream: currentUpstream,
		UpstreamPodName: upstreamPodName,
		UpstreamHealthy: upstreamHealthy,
		ConnectionStats: httpapi.GatewayConnectionStats{
			ActiveConnections:   gatewayStats.ActiveConnections,
			TotalConnections:    gatewayStats.TotalConnections,
			RejectedConnections: gatewayStats.RejectedConnections,
			Errors:              gatewayStats.Errors,
		},
		HealthCheckStats: healthCheckStats,
	}
}

// getUpstreamPodName extracts the pod name from an upstream address
func (c *MemgraphController) getUpstreamPodName(upstreamAddress string) string {
	// Parse IP from address (format: "IP:7687")
	if len(upstreamAddress) < 6 || upstreamAddress[len(upstreamAddress)-5:] != ":7687" {
		return ""
	}

	// Find the pod with matching address
	for podName, node := range c.cluster.MemgraphNodes {
		if nodeAddress, err := node.GetBoltAddress(); err == nil {
			if nodeAddress == upstreamAddress {
				return podName
			}
		}
	}

	return ""
}
