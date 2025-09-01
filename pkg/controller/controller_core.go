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

	// State management
	targetMainIndex int
	configMapName   string
	targetMutex     sync.RWMutex

	// Cluster operations
	cluster *MemgraphCluster

	// In-memory cluster state for immediate event processing
	stateMutex       sync.RWMutex
	stateLastUpdated time.Time

	// Controller loop state
	isRunning   bool
	mu          sync.RWMutex
	maxFailures int

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

	// Initialize state management with release-based ConfigMap name
	controller.configMapName = generateStateConfigMapName()
	controller.targetMainIndex = -1 // -1 indicates not yet loaded from ConfigMap

	// Initialize cluster operations
	controller.cluster = NewMemgraphCluster(clientset, config, controller.memgraphClient)

	// Initialize controller state
	controller.maxFailures = 5
	controller.stopCh = make(chan struct{})

	// Initialize reconciliation metrics
	controller.metrics = &ReconciliationMetrics{}

	// Set up pod informer for event-driven reconciliation
	controller.setupInformers()

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
		return 0, fmt.Errorf("ConfigMap not available: %w", err)
	}

	targetMainIndexStr, exists := configMap.Data["targetMainIndex"]
	if !exists {
		return 0, fmt.Errorf("targetMainIndex not found in ConfigMap")
	}

	var targetMainIndex int
	if _, err := fmt.Sscanf(targetMainIndexStr, "%d", &targetMainIndex); err != nil {
		return 0, fmt.Errorf("invalid targetMainIndex format: %w", err)
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

// updateCachedState updates the in-memory cluster state
func (c *MemgraphController) updateCachedState(cluster *MemgraphCluster) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()

	// Deep copy the cluster state for thread safety
	c.cluster = cluster
	c.stateLastUpdated = time.Now()
}

// getCachedState returns the current cached cluster state
func (c *MemgraphController) getCachedState() (*MemgraphCluster, time.Time) {
	c.stateMutex.RLock()
	defer c.stateMutex.RUnlock()
	return c.cluster, c.stateLastUpdated
}

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
	err := c.cluster.DiscoverPods(ctx)
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

// GetCurrentMainEndpoint returns the current main endpoint for the gateway
func (c *MemgraphController) GetCurrentMainEndpoint(ctx context.Context) (string, error) {
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get target main index: %w", err)
	}

	podName := c.config.GetPodName(targetMainIndex)

	// Get the actual pod IP instead of using FQDN to avoid DNS refresh timing issues
	pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get pod %s: %w", podName, err)
	}

	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod %s has no IP address assigned", podName)
	}

	endpoint := fmt.Sprintf("%s:7687", pod.Status.PodIP)

	return endpoint, nil
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
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced, c.configMapInformer.HasSynced) {
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
	if c.gatewayServer != nil {
		return c.gatewayServer.Start(ctx)
	}
	return nil
}

// StopGatewayServer stops the gateway server
func (c *MemgraphController) StopGatewayServer(ctx context.Context) error {
	if c.gatewayServer != nil {
		return c.gatewayServer.Stop(ctx)
	}
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

	// Stop gateway server
	if c.gatewayServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.gatewayServer.Stop(ctx); err != nil {
			log.Printf("Error stopping gateway server: %v", err)
		}
	}

	log.Println("Memgraph Controller stopped")
}

// getPodIPEndpoint gets the IP-based endpoint for a pod by index
func (c *MemgraphController) getPodIPEndpoint(ctx context.Context, podIndex int) (string, error) {
	podName := c.config.GetPodName(podIndex)

	pod, err := c.clientset.CoreV1().Pods(c.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get pod %s: %w", podName, err)
	}

	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("pod %s has no IP address assigned", podName)
	}

	return pod.Status.PodIP, nil
}

// GetControllerStatus returns the current status of the controller
func (c *MemgraphController) GetControllerStatus() map[string]interface{} {
	return map[string]interface{}{
		"is_leader":  c.IsLeader(),
		"is_running": c.IsRunning(),
		"metrics":    c.GetReconciliationMetrics(),
	}
}
