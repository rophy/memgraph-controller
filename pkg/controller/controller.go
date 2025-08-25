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

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
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
	clientset      kubernetes.Interface
	config         *Config
	podDiscovery   *PodDiscovery
	memgraphClient *MemgraphClient
	httpServer     *HTTPServer
	gatewayServer  GatewayServerInterface

	// Controller loop state
	isRunning     bool
	mu            sync.RWMutex
	lastReconcile time.Time
	failureCount  int
	maxFailures   int

	// Controller state tracking
	lastKnownMain   string
	targetMainIndex int
	isBootstrap     bool // Always starts as true, set to false after bootstrap completes

	// Event-driven reconciliation
	podInformer     cache.SharedInformer
	informerFactory informers.SharedInformerFactory
	workQueue       chan reconcileRequest
	stopCh          chan struct{}

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
		podDiscovery:   NewPodDiscovery(clientset, config),
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

	// Initialize controller state
	controller.maxFailures = 5
	controller.targetMainIndex = -1
	controller.isBootstrap = true // Controller starts in bootstrap phase
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

// detectMainFailover detects if current main has failed and promotion is needed

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
	log.Printf("  - Current main: %s", clusterState.CurrentMain)
	log.Printf("  - State type: %s", clusterState.StateType.String())
	log.Printf("  - Target main index: %d", clusterState.TargetMainIndex)

	// Log pod states
	for podName, podInfo := range clusterState.Pods {
		log.Printf("  - Pod %s: State=%s, MemgraphRole=%s, Replicas=%d",
			podName, podInfo.State, podInfo.MemgraphRole, len(podInfo.Replicas))
	}

	// Operational phase: detect failover and enforce expected topology
	if !clusterState.IsBootstrapPhase {
		// Check for main failover first
		if c.detectMainFailover(clusterState) {
			log.Printf("Main failover detected - initiating failover procedure...")
			if err := c.handleMainFailover(ctx, clusterState); err != nil {
				log.Printf("❌ Main failover failed: %v", err)
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
			CurrentMain:           clusterState.CurrentMain,
			CurrentSyncReplica:    currentSyncReplica,
			TotalPods:             totalPods,
			HealthyPods:           healthyPods,
			UnhealthyPods:         unhealthyPods,
			SyncReplicaHealthy:    syncReplicaHealthy,
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

	c.enqueuePodEvent("pod-deleted")
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

	return NewPodInfo(pod, c.config.ServiceName), nil
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

	// Start gateway server only if not in bootstrap phase
	if c.gatewayServer != nil && !c.gatewayServer.IsBootstrapPhase() {
		log.Println("Starting gateway server...")
		if err := c.gatewayServer.Start(ctx); err != nil {
			log.Printf("Failed to start gateway server: %v", err)
			c.stop()
			return fmt.Errorf("failed to start gateway server: %w", err)
		}
	} else {
		log.Println("Gateway server start deferred - currently in bootstrap phase")
	}

	// Initial reconciliation
	c.enqueueReconcile("initial-reconciliation")

	// Start worker goroutine
	const numWorkers = 1
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

	// Stop gateway server
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.gatewayServer.Stop(shutdownCtx); err != nil {
		log.Printf("Error stopping gateway server: %v", err)
	}

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
		"running":         c.isRunning,
		"last_reconcile":  c.lastReconcile,
		"failure_count":   c.failureCount,
		"max_failures":    c.maxFailures,
		"work_queue_size": len(c.workQueue),
	}
}

// GetCurrentMainEndpoint returns the current main endpoint for gateway connections
func (c *MemgraphController) GetCurrentMainEndpoint(ctx context.Context) (string, error) {
	c.mu.RLock()
	currentMain := c.lastKnownMain
	c.mu.RUnlock()

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
func (c *MemgraphController) updateGatewayMain() {
	// Skip if gateway server is not initialized (e.g., in tests)
	if c.gatewayServer == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	endpoint, err := c.GetCurrentMainEndpoint(ctx)
	if err != nil {
		log.Printf("Gateway: Failed to get main endpoint for notification: %v", err)
		return
	}

	// Update the gateway's main endpoint
	c.gatewayServer.SetCurrentMain(endpoint)
}

// handleMainFailurePromotion promotes SYNC replica when main fails during operations

// promoteToMain promotes a pod to main role
func (c *MemgraphController) promoteToMain(podName string) error {
	log.Printf("Promoting pod %s to main role", podName)

	// Discover current cluster state to get pod information
	ctx := context.Background()
	clusterState, err := c.podDiscovery.DiscoverPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover pods: %w", err)
	}

	// Get pod information from cluster state
	podInfo, exists := clusterState.Pods[podName]
	if !exists {
		return fmt.Errorf("pod %s not found in cluster state", podName)
	}

	// Use the bolt address from pod info
	boltAddress := podInfo.BoltAddress

	// Execute promotion command using MemgraphClient pattern
	err = WithRetry(ctx, func() error {
		driver, err := c.memgraphClient.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
			}
		}()

		// Execute promotion command
		_, err = session.Run(ctx, "SET REPLICATION ROLE TO MAIN;", nil)
		if err != nil {
			return fmt.Errorf("failed to execute SET REPLICATION ROLE TO MAIN: %w", err)
		}

		return nil
	}, c.memgraphClient.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to promote %s to main: %w", podName, err)
	}

	log.Printf("✅ Successfully promoted %s to main role", podName)
	return nil
}

// enforceMainAuthority enforces the current main and demotes incorrect mains
func (c *MemgraphController) enforceMainAuthority(clusterState *ClusterState, mainPods []string) error {
	// Determine which main to keep (controller authority)
	correctMain := c.lastKnownMain
	if correctMain == "" {
		// Fallback: use lower-index precedence if no known main
		correctMain = c.selectLowestIndexMain(mainPods)
	}

	log.Printf("Enforcing main authority: correct_main=%s, current_mains=%v", correctMain, mainPods)

	// Demote all incorrect mains
	var demotedPods []string
	for _, mainPod := range mainPods {
		if mainPod != correctMain {
			log.Printf("Demoting incorrect main %s to replica", mainPod)
			if err := c.demoteToReplica(mainPod); err != nil {
				log.Printf("Warning: failed to demote %s: %v", mainPod, err)
				continue
			}
			demotedPods = append(demotedPods, mainPod)
		}
	}

	// Update controller state tracking
	c.lastKnownMain = correctMain
	c.targetMainIndex = c.config.ExtractPodIndex(correctMain)

	// Notify gateway of main change (async to avoid blocking reconciliation)
	go c.updateGatewayMain()

	log.Printf("✅ Split-brain resolved: main=%s, demoted=%v", correctMain, demotedPods)
	return nil
}

// selectLowestIndexMain selects the main with the lowest pod index
func (c *MemgraphController) selectLowestIndexMain(mainPods []string) string {
	if len(mainPods) == 0 {
		return ""
	}

	lowestMain := mainPods[0]
	lowestIndex := c.config.ExtractPodIndex(lowestMain)

	for _, main := range mainPods[1:] {
		index := c.config.ExtractPodIndex(main)
		if index >= 0 && index < lowestIndex {
			lowestMain = main
			lowestIndex = index
		}
	}

	log.Printf("Lower-index precedence: selected %s (index=%d) from %v", lowestMain, lowestIndex, mainPods)
	return lowestMain
}

// demoteToReplica demotes a pod from main to replica role
func (c *MemgraphController) demoteToReplica(podName string) error {
	log.Printf("Demoting pod %s from main to replica", podName)

	// Discover current cluster state to get pod information
	ctx := context.Background()
	clusterState, err := c.podDiscovery.DiscoverPods(ctx)
	if err != nil {
		return fmt.Errorf("failed to discover pods: %w", err)
	}

	// Get pod information from cluster state
	podInfo, exists := clusterState.Pods[podName]
	if !exists {
		return fmt.Errorf("pod %s not found in cluster state", podName)
	}

	// Use the bolt address from pod info
	boltAddress := podInfo.BoltAddress

	// Execute demotion command
	err = WithRetry(ctx, func() error {
		driver, err := c.memgraphClient.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
			}
		}()

		// Execute demotion command with required port for Memgraph Community Edition
		_, err = session.Run(ctx, "SET REPLICATION ROLE TO REPLICA WITH PORT 10000;", nil)
		if err != nil {
			return fmt.Errorf("failed to execute SET REPLICATION ROLE TO REPLICA: %w", err)
		}

		return nil
	}, c.memgraphClient.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to demote %s to replica: %w", podName, err)
	}

	log.Printf("✅ Successfully demoted %s to replica role", podName)
	return nil
}
