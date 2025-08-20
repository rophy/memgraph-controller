package controller

import (
	"context"
	"fmt"
	"log"
	"math"
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
				// Extract replica names for the PodInfo
				var replicaNames []string
				for _, replica := range replicasResp.Replicas {
					replicaNames = append(replicaNames, replica.Name)
				}
				podInfo.Replicas = replicaNames
				log.Printf("Pod %s has %d replicas: %v", podName, len(replicaNames), replicaNames)
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

	// Phase 2: Register replicas with master
	masterPod, exists := clusterState.Pods[currentMaster]
	if !exists {
		return fmt.Errorf("master pod %s not found in cluster state", currentMaster)
	}

	log.Printf("Registering replicas with master %s", currentMaster)
	
	for podName, podInfo := range clusterState.Pods {
		// Skip the master pod itself
		if podName == currentMaster {
			continue
		}

		// Register all non-master pods as replicas (they should be connected to master)
		// This includes both pods that need to become replicas and existing replicas
		// that need to be registered with the (potentially new) master

		replicaName := podInfo.GetReplicaName()
		replicaAddress := podInfo.GetReplicationAddress(c.config.ServiceName)
		
		log.Printf("Registering replica %s (pod %s) at %s with master %s", 
			replicaName, podName, replicaAddress, currentMaster)

		if err := c.memgraphClient.RegisterReplicaWithRetry(ctx, masterPod.BoltAddress, replicaName, replicaAddress); err != nil {
			log.Printf("Failed to register replica %s with master: %v", replicaName, err)
			configErrors = append(configErrors, fmt.Errorf("register replica %s: %w", replicaName, err))
			continue
		}

		log.Printf("Successfully registered replica %s with master %s (ASYNC mode)", replicaName, currentMaster)
	}

	// Phase 3: Handle any existing replicas that should be removed
	if err := c.cleanupObsoleteReplicas(ctx, clusterState); err != nil {
		log.Printf("Warning: failed to cleanup obsolete replicas: %v", err)
		configErrors = append(configErrors, fmt.Errorf("cleanup obsolete replicas: %w", err))
	}

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

// cleanupObsoleteReplicas removes replica registrations that are no longer needed
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

	// Build set of expected replica names
	// All non-master pods should be registered as replicas
	expectedReplicas := make(map[string]bool)
	for podName, podInfo := range clusterState.Pods {
		if podName != currentMaster {
			expectedReplicas[podInfo.GetReplicaName()] = true
		}
	}

	// Remove any replicas that shouldn't exist
	var cleanupErrors []error
	for _, replica := range replicasResp.Replicas {
		if !expectedReplicas[replica.Name] {
			log.Printf("Dropping obsolete replica %s from master %s", replica.Name, currentMaster)
			if err := c.memgraphClient.DropReplicaWithRetry(ctx, masterPod.BoltAddress, replica.Name); err != nil {
				log.Printf("Failed to drop obsolete replica %s: %v", replica.Name, err)
				cleanupErrors = append(cleanupErrors, fmt.Errorf("drop replica %s: %w", replica.Name, err))
			} else {
				log.Printf("Successfully dropped obsolete replica %s", replica.Name)
			}
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
			CurrentMaster:  clusterState.CurrentMaster,
			TotalPods:      totalPods,
			HealthyPods:    healthyPods,
			UnhealthyPods:  unhealthyPods,
		},
		Pods: podStatuses,
	}

	log.Printf("Cluster status collected: %d total pods, %d healthy, %d unhealthy, master: %s", 
		totalPods, healthyPods, unhealthyPods, clusterState.CurrentMaster)

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