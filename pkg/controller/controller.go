package controller

import (
	"context"
	"fmt"
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
		if role.Role == "MAIN" {
			log.Printf("Querying replicas for MAIN pod %s", podName)
			
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
	
	log.Printf("Reconciliation cycle completed successfully. Current cluster state:")
	log.Printf("  - Total pods: %d", len(clusterState.Pods))
	log.Printf("  - Current master: %s", clusterState.CurrentMaster)
	
	// Log pod states
	for podName, podInfo := range clusterState.Pods {
		log.Printf("  - Pod %s: State=%s, K8sRole=%s, MemgraphRole=%s, Replicas=%d", 
			podName, podInfo.State, podInfo.KubernetesRole, podInfo.MemgraphRole, len(podInfo.Replicas))
	}
	
	return nil
}