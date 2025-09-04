package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"memgraph-controller/pkg/gateway"
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
	gatewayServer  *gateway.Server

	// Leader election
	leaderElection  *LeaderElection
	isLeader        bool
	lastKnownLeader string // Track last known leader to detect actual changes
	leaderMu        sync.RWMutex

	// State management
	targetMainIndex int
	configMapName   string
	targetMutex     sync.RWMutex

	// Event-driven reconciliation
	reconcileQueue     *ReconcileQueue
	failoverCheckQueue *FailoverCheckQueue

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

	// Reconciliation metrics
	metrics *ReconciliationMetrics
}

func NewMemgraphController(clientset kubernetes.Interface, config *Config) *MemgraphController {
	controller := &MemgraphController{
		clientset:      clientset,
		config:         config,
		memgraphClient: NewMemgraphClient(config),
	}

	// Initialize HTTP server
	controller.httpServer = NewHTTPServer(controller, config)

	// Initialize gateway server (always enabled)
	gatewayConfig := gateway.LoadGatewayConfig()
	gatewayConfig.Enabled = true
	gatewayConfig.BindAddress = config.GatewayBindAddress

	// Create bootstrap phase provider
	bootstrapProvider := func() bool {
		return !controller.isLeader || controller.targetMainIndex == -1
	}

	// Create main node provider that converts controller.MemgraphNode to gateway.MemgraphNode
	mainNodeProvider := func(ctx context.Context) (*gateway.MemgraphNode, error) {
		controllerNode, err := controller.GetCurrentMainNode(ctx)
		if err != nil {
			return nil, err
		}
		if controllerNode == nil {
			return nil, fmt.Errorf("no main node available")
		}
		pod, err := controller.getPodFromCache(controllerNode.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to get pod %s from cache: %w", controllerNode.Name, err)
		}
		return &gateway.MemgraphNode{
			Name:        controllerNode.Name,
			BoltAddress: controllerNode.BoltAddress,
			Pod:         pod,
		}, nil
	}

	controller.gatewayServer = gateway.NewServer(gatewayConfig, mainNodeProvider, bootstrapProvider)

	// Initialize leader election
	controller.leaderElection = NewLeaderElection(clientset, config)
	controller.setupLeaderElectionCallbacks()

	// Initialize state management with release-based ConfigMap name
	controller.configMapName = generateStateConfigMapName()
	controller.targetMainIndex = -1 // -1 indicates not yet loaded from ConfigMap


	// Initialize event-driven reconciliation queue
	controller.reconcileQueue = controller.newReconcileQueue()
	controller.failoverCheckQueue = controller.newFailoverCheckQueue()

	// Initialize controller state
	controller.maxFailures = 5
	controller.stopCh = make(chan struct{})

	// Initialize reconciliation metrics
	controller.metrics = &ReconciliationMetrics{}

	// Set up pod informer for event-driven reconciliation
	controller.setupInformers()

	// Initialize cluster operations (after informers are set up)
	controller.cluster = NewMemgraphCluster(controller.podInformer.GetStore(), config, controller.memgraphClient)

	return controller
}

// GetTargetMainIndex returns the target main index from ConfigMap or error if not available
func (c *MemgraphController) GetTargetMainIndex(ctx context.Context) (int, error) {
	if c.targetMainIndex != -1 {
		return c.targetMainIndex, nil
	}

	// Need to load from ConfigMap
	c.targetMutex.Lock()
	defer c.targetMutex.Unlock()

	// Load state from ConfigMap
	configMap, err := c.clientset.CoreV1().ConfigMaps(c.config.Namespace).Get(ctx, c.configMapName, metav1.GetOptions{})
	if err != nil {
		return -1, fmt.Errorf("ConfigMap not available: %w", err)
	}

	targetMainIndexStr, exists := configMap.Data["targetMainIndex"]
	if !exists {
		return -1, fmt.Errorf("targetMainIndex not found in ConfigMap")
	}

	var targetMainIndex int
	if _, err := fmt.Sscanf(targetMainIndexStr, "%d", &targetMainIndex); err != nil {
		return -1, fmt.Errorf("invalid targetMainIndex format: %w", err)
	}

	c.targetMainIndex = targetMainIndex
	return targetMainIndex, nil
}

// SetTargetMainIndex updates both in-memory target and ConfigMap
func (c *MemgraphController) SetTargetMainIndex(ctx context.Context, index int) error {

	c.targetMutex.Lock()
	defer c.targetMutex.Unlock()

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

	_, err := c.clientset.CoreV1().ConfigMaps(c.config.Namespace).Update(ctx, configMap, metav1.UpdateOptions{})
	if err != nil {
		// Try to create if it doesn't exist
		if errors.IsNotFound(err) {
			_, err = c.clientset.CoreV1().ConfigMaps(c.config.Namespace).Create(ctx, configMap, metav1.CreateOptions{})
		}
		if err != nil {
			return fmt.Errorf("failed to update ConfigMap: %w", err)
		}
	}

	// Update in-memory value
	c.targetMainIndex = index
	log.Printf("Updated TargetMainIndex to %d", index)
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
	var targetSyncIndex int
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

// updateCachedState removed - obsolete with new architecture

// isPodBecomeUnhealthy checks if a pod transitioned from healthy to unhealthy
func (c *MemgraphController) isPodBecomeUnhealthy(oldPod, newPod *v1.Pod) bool {
	if oldPod == nil || newPod == nil {
		return false
	}

	oldReady := isPodReady(oldPod)
	newReady := isPodReady(newPod)

	// Pod became unhealthy if it was ready before and is not ready now
	if oldReady && !newReady {
		log.Printf("Pod %s became unhealthy: ready %v -> %v", newPod.Name, oldReady, newReady)
		return true
	}

	return false
}

// isPodReady checks if a pod is ready based on its conditions
func isPodReady(pod *v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

// IsLeader returns whether this controller instance is the current leader
func (c *MemgraphController) IsLeader() bool {
	c.leaderMu.RLock()
	defer c.leaderMu.RUnlock()
	return c.isLeader
}

// IsRunning returns whether the controller is currently running
func (c *MemgraphController) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isRunning
}

// GetLeaderElection returns the leader election instance for testing
func (c *MemgraphController) GetLeaderElection() *LeaderElection {
	return c.leaderElection
}

// TestConnection tests basic connectivity to Kubernetes API
func (c *MemgraphController) TestConnection() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.clientset.CoreV1().Pods(c.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", c.config.AppName),
		Limit:         1,
	})

	if err != nil {
		return fmt.Errorf("failed to connect to Kubernetes API: %w", err)
	}

	log.Printf("Successfully connected to Kubernetes API. Found pods with app.kubernetes.io/name=%s in namespace %s",
		c.config.AppName, c.config.Namespace)
	return nil
}

// TestMemgraphConnections tests connections to all discovered Memgraph pods
func (c *MemgraphController) TestMemgraphConnections(ctx context.Context) error {
	err := c.cluster.DiscoverPods(ctx, c.getPodsFromCache)
	if err != nil {
		return fmt.Errorf("failed to discover pods: %w", err)
	}
	pods := c.cluster.MemgraphNodes

	if len(pods) == 0 {
		return fmt.Errorf("no pods found with label app.kubernetes.io/name=%s", c.config.AppName)
	}

	log.Printf("Testing Memgraph connections to %d pods...", len(pods))

	var lastErr error
	connectedCount := 0
	for podName, pod := range pods {
		endpoint := pod.BoltAddress
		if err := c.memgraphClient.TestConnection(ctx, endpoint); err != nil {
			log.Printf("❌ Failed to connect to %s (%s): %v", podName, endpoint, err)
			lastErr = err
		} else {
			log.Printf("✅ Successfully connected to %s (%s)", podName, endpoint)
			connectedCount++
		}
	}

	if connectedCount == 0 {
		return fmt.Errorf("failed to connect to any Memgraph pods: %w", lastErr)
	}

	log.Printf("Successfully tested Memgraph connections: %d/%d pods accessible", connectedCount, len(pods))
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
		if node.MemgraphRole == "main" {
			// Check if pod exists in cache and has IP
			pod, err := c.getPodFromCache(node.Name)
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
		_, err := c.getPodFromCache(node.Name)
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

// StartHTTPServer starts the HTTP server
func (c *MemgraphController) StartHTTPServer() error {
	log.Println("Starting HTTP server...")
	return c.httpServer.Start()
}

// StopHTTPServer stops the HTTP server gracefully
func (c *MemgraphController) StopHTTPServer(ctx context.Context) error {
	log.Println("Stopping HTTP server...")
	if c.httpServer != nil {
		return c.httpServer.Stop(ctx)
	}
	return nil
}

// StartInformers starts the Kubernetes informers and waits for cache sync
func (c *MemgraphController) StartInformers() error {
	c.informerFactory.Start(c.stopCh)

	// Wait for informer caches to sync
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer caches")
	}
	return nil
}

// StopInformers stops the Kubernetes informers
func (c *MemgraphController) StopInformers() {
	if c.stopCh != nil {
		close(c.stopCh)
	}
}

// StartGatewayServer starts the gateway server
func (c *MemgraphController) StartGatewayServer(ctx context.Context) error {
	return c.gatewayServer.Start(ctx)
}

// StopGatewayServer stops the gateway server (no-op - process termination handles cleanup)
func (c *MemgraphController) StopGatewayServer(ctx context.Context) error {
	log.Println("Gateway: No explicit shutdown needed - process termination handles cleanup")
	return nil
}

// RunLeaderElection runs the leader election process
func (c *MemgraphController) RunLeaderElection(ctx context.Context) error {
	if c.leaderElection != nil {
		return c.leaderElection.Run(ctx)
	}
	return nil
}

// stop performs cleanup when the controller stops
func (c *MemgraphController) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.isRunning {
		return // Already stopped
	}

	log.Println("Stopping Memgraph Controller...")
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
	log.Println("Gateway: Cleanup will be handled by process termination")

	log.Println("Memgraph Controller stopped")
}

// GetControllerStatus returns the current status of the controller
func (c *MemgraphController) GetControllerStatus() map[string]interface{} {
	return map[string]interface{}{
		"is_leader":  c.IsLeader(),
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

// getPodsFromCache retrieves all memgraph pods from the informer cache
func (c *MemgraphController) getPodsFromCache() []v1.Pod {
	objects := c.podInformer.GetStore().List()
	pods := make([]v1.Pod, 0, len(objects))
	
	for _, obj := range objects {
		if pod, ok := obj.(*v1.Pod); ok {
			pods = append(pods, *pod)
		}
	}
	
	return pods
}
