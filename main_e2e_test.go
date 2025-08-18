//go:build e2e

package main

import (
	"context"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func TestE2E_KubernetesConnection(t *testing.T) {
	config, err := getKubernetesConfig()
	if err != nil {
		t.Skipf("Skipping e2e test: could not get Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	controller := &MemgraphController{
		clientset: clientset,
		config: &Config{
			AppName:   "memgraph",
			Namespace: "memgraph",
		},
	}

	err = controller.testConnection()
	if err != nil {
		t.Errorf("Failed to connect to Kubernetes cluster: %v", err)
	}
}

func TestE2E_PodDiscovery(t *testing.T) {
	config, err := getKubernetesConfig()
	if err != nil {
		t.Skipf("Skipping e2e test: could not get Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	controller := &MemgraphController{
		clientset: clientset,
		config: &Config{
			AppName:   "memgraph",
			Namespace: "memgraph",
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pods, err := controller.clientset.CoreV1().Pods(controller.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + controller.config.AppName,
	})
	if err != nil {
		t.Errorf("Failed to list pods: %v", err)
		return
	}

	t.Logf("Found %d pods with app=%s in namespace %s",
		len(pods.Items), controller.config.AppName, controller.config.Namespace)

	for _, pod := range pods.Items {
		t.Logf("Pod: %s, Phase: %s, Ready: %v",
			pod.Name, pod.Status.Phase, isPodReady(&pod))
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