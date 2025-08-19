package controller

import (
	"context"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type MemgraphController struct {
	clientset       kubernetes.Interface
	config          *Config
	podDiscovery    *PodDiscovery
	memgraphClient  *MemgraphClient
}

func NewMemgraphController(clientset kubernetes.Interface, config *Config) *MemgraphController {
	return &MemgraphController{
		clientset:      clientset,
		config:         config,
		podDiscovery:   NewPodDiscovery(clientset, config),
		memgraphClient: NewMemgraphClient(config),
	}
}

func (c *MemgraphController) TestConnection() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pods, err := c.clientset.CoreV1().Pods(c.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + c.config.AppName,
	})
	if err != nil {
		return err
	}

	log.Printf("Successfully connected to Kubernetes API. Found %d pods with app=%s in namespace %s",
		len(pods.Items), c.config.AppName, c.config.Namespace)

	return nil
}

func (c *MemgraphController) DiscoverCluster(ctx context.Context) (*ClusterState, error) {
	log.Println("Discovering Memgraph cluster...")
	
	clusterState, err := c.podDiscovery.DiscoverPods(ctx)
	if err != nil {
		return nil, err
	}

	log.Printf("Discovered %d pods, current master: %s", len(clusterState.Pods), clusterState.CurrentMaster)

	// Query Memgraph role for each pod
	for podName, podInfo := range clusterState.Pods {
		if podInfo.BoltAddress == "" {
			log.Printf("Skipping pod %s: no Bolt address", podName)
			continue
		}

		log.Printf("Querying replication role for pod %s at %s", podName, podInfo.BoltAddress)
		
		role, err := c.memgraphClient.QueryReplicationRole(ctx, podInfo.BoltAddress)
		if err != nil {
			log.Printf("Failed to query replication role for pod %s: %v", podName, err)
			// Continue with other pods even if one fails
			continue
		}

		podInfo.MemgraphRole = role.Role
		log.Printf("Pod %s has Memgraph role: %s", podName, role.Role)
	}

	return clusterState, nil
}

func (c *MemgraphController) TestMemgraphConnections(ctx context.Context) error {
	clusterState, err := c.podDiscovery.DiscoverPods(ctx)
	if err != nil {
		return err
	}

	log.Printf("Testing Memgraph connections for %d pods...", len(clusterState.Pods))

	for podName, podInfo := range clusterState.Pods {
		if podInfo.BoltAddress == "" {
			log.Printf("Skipping pod %s: no Bolt address", podName)
			continue
		}

		log.Printf("Testing connection to pod %s at %s", podName, podInfo.BoltAddress)
		
		err := c.memgraphClient.TestConnection(ctx, podInfo.BoltAddress)
		if err != nil {
			log.Printf("Failed to connect to pod %s: %v", podName, err)
			// Continue testing other pods
			continue
		}

		log.Printf("Successfully connected to pod %s", podName)
	}

	return nil
}