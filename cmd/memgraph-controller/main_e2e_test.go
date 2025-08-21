//go:build e2e

package main

import (
	"context"
	"testing"
	"time"

	"memgraph-controller/pkg/controller"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

func TestE2E_KubernetesConnection(t *testing.T) {
	k8sConfig, err := controller.GetKubernetesConfig()
	if err != nil {
		t.Skipf("Skipping e2e test: could not get Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		t.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	ctrl := controller.NewMemgraphController(clientset, &controller.Config{
		AppName:   "memgraph",
		Namespace: "memgraph",
	})

	err = ctrl.TestConnection()
	if err != nil {
		t.Errorf("Failed to connect to Kubernetes cluster: %v", err)
	}
}

func TestE2E_PodDiscovery(t *testing.T) {
	k8sConfig, err := controller.GetKubernetesConfig()
	if err != nil {
		t.Skipf("Skipping e2e test: could not get Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		t.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	ctrlConfig := &controller.Config{
		AppName:     "memgraph",
		Namespace:   "memgraph", 
		ServiceName: "memgraph",
	}

	ctrl := controller.NewMemgraphController(clientset, ctrlConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Test controller pod discovery
	clusterState, err := ctrl.DiscoverCluster(ctx)
	if err != nil {
		t.Fatalf("Failed to discover cluster: %v", err)
	}

	// Validate that pods were found
	if len(clusterState.Pods) == 0 {
		t.Fatal("Expected to find Memgraph pods, but found 0")
	}

	t.Logf("Found %d pods in cluster", len(clusterState.Pods))

	// Validate each discovered pod
	for podName, podInfo := range clusterState.Pods {
		t.Logf("Validating pod: %s", podName)

		// Pod must have a valid name
		if podInfo.Name == "" {
			t.Errorf("Pod has empty name")
		}

		// Pod must have a bolt address (IP:7687)
		if podInfo.BoltAddress == "" {
			t.Errorf("Pod %s has empty BoltAddress", podName)
		}

		// Pod must have a replication address  
		expectedReplicationAddr := podName + "." + ctrlConfig.ServiceName + ":10000"
		if podInfo.ReplicationAddress != expectedReplicationAddr {
			t.Errorf("Pod %s ReplicationAddress = %s, want %s", 
				podName, podInfo.ReplicationAddress, expectedReplicationAddr)
		}

		// Pod must have replica name (dashes converted to underscores)
		if podInfo.ReplicaName == "" {
			t.Errorf("Pod %s has empty ReplicaName", podName)
		}

		// Pod must have a valid timestamp
		if podInfo.Timestamp.IsZero() {
			t.Errorf("Pod %s has zero timestamp", podName)
		}

		// Pod must be in Running phase
		if podInfo.Pod.Status.Phase != v1.PodRunning {
			t.Errorf("Pod %s is in phase %s, expected Running", podName, podInfo.Pod.Status.Phase)
		}

		// Pod must have an IP address
		if podInfo.Pod.Status.PodIP == "" {
			t.Errorf("Pod %s has no IP address", podName)
		}

		t.Logf("Pod %s: Phase=%s, IP=%s, BoltAddr=%s", 
			podName, podInfo.Pod.Status.Phase, podInfo.Pod.Status.PodIP, 
			podInfo.BoltAddress)
	}

	// Validate master selection
	if clusterState.CurrentMaster == "" {
		t.Error("No master was selected from discovered pods")
	} else {
		// Verify the selected master exists in the pod list
		if _, exists := clusterState.Pods[clusterState.CurrentMaster]; !exists {
			t.Errorf("Selected master %s not found in pod list", clusterState.CurrentMaster)
		}
		t.Logf("Selected master: %s", clusterState.CurrentMaster)
	}
}

func TestE2E_MemgraphConnectivity(t *testing.T) {
	k8sConfig, err := controller.GetKubernetesConfig()
	if err != nil {
		t.Skipf("Skipping e2e test: could not get Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		t.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	ctrlConfig := &controller.Config{
		AppName:     "memgraph",
		Namespace:   "memgraph",
		ServiceName: "memgraph",
	}

	ctrl := controller.NewMemgraphController(clientset, ctrlConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Discover pods first
	clusterState, err := ctrl.DiscoverCluster(ctx)
	if err != nil {
		t.Fatalf("Failed to discover cluster: %v", err)
	}

	if len(clusterState.Pods) == 0 {
		t.Skip("No Memgraph pods found, skipping connectivity test")
	}

	// Test connectivity to each pod
	connectivityErrors := 0
	for podName, podInfo := range clusterState.Pods {
		if podInfo.BoltAddress == "" {
			t.Logf("Skipping pod %s: no Bolt address", podName)
			continue
		}

		t.Logf("Testing connectivity to pod %s at %s", podName, podInfo.BoltAddress)
		
		// Try to connect with a shorter timeout for individual pods
		podCtx, podCancel := context.WithTimeout(ctx, 10*time.Second)
		err := ctrl.TestMemgraphConnections(podCtx)
		podCancel()

		if err != nil {
			t.Logf("Connectivity test failed for pod %s: %v", podName, err)
			connectivityErrors++
		} else {
			t.Logf("Successfully connected to pod %s", podName)
		}
	}

	// Allow some connectivity failures but not total failure
	if connectivityErrors == len(clusterState.Pods) {
		t.Error("Failed to connect to any Memgraph pods - all connectivity tests failed")
	} else if connectivityErrors > 0 {
		t.Logf("Warning: %d/%d pod connectivity tests failed", connectivityErrors, len(clusterState.Pods))
	}
}

func isPodReady(pod *v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}