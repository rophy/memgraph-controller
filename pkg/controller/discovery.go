package controller

import (
	"context"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type PodDiscovery struct {
	clientset kubernetes.Interface
	config    *Config
}

func NewPodDiscovery(clientset kubernetes.Interface, config *Config) *PodDiscovery {
	return &PodDiscovery{
		clientset: clientset,
		config:    config,
	}
}

func (pd *PodDiscovery) DiscoverPods(ctx context.Context) (*ClusterState, error) {
	pods, err := pd.clientset.CoreV1().Pods(pd.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + pd.config.AppName,
	})
	if err != nil {
		return nil, err
	}

	clusterState := NewClusterState()

	for _, pod := range pods.Items {
		// Only process running pods
		if pod.Status.Phase != "Running" {
			log.Printf("Skipping pod %s in phase %s", pod.Name, pod.Status.Phase)
			continue
		}

		// Only process pods with IP assigned
		if pod.Status.PodIP == "" {
			log.Printf("Skipping pod %s without IP address", pod.Name)
			continue
		}

		podInfo := NewPodInfo(&pod, pd.config.ServiceName)
		clusterState.Pods[pod.Name] = podInfo

		log.Printf("Discovered pod: %s, IP: %s, Role: %s, Timestamp: %s",
			podInfo.Name,
			pod.Status.PodIP,
			podInfo.KubernetesRole,
			podInfo.Timestamp.Format(time.RFC3339))
	}

	// Determine current master based on latest timestamp
	pd.selectMaster(clusterState)

	return clusterState, nil
}

func (pd *PodDiscovery) selectMaster(clusterState *ClusterState) {
	var latestPod *PodInfo
	var latestTime time.Time

	// Find the pod with the latest timestamp
	for _, podInfo := range clusterState.Pods {
		if latestPod == nil || podInfo.Timestamp.After(latestTime) {
			latestPod = podInfo
			latestTime = podInfo.Timestamp
		}
	}

	if latestPod != nil {
		clusterState.CurrentMaster = latestPod.Name
		log.Printf("Selected master: %s (timestamp: %s)",
			latestPod.Name,
			latestPod.Timestamp.Format(time.RFC3339))
	}
}

func (pd *PodDiscovery) GetPodsByLabel(ctx context.Context, labelSelector string) (*ClusterState, error) {
	pods, err := pd.clientset.CoreV1().Pods(pd.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	clusterState := NewClusterState()

	for _, pod := range pods.Items {
		podInfo := NewPodInfo(&pod, pd.config.ServiceName)
		clusterState.Pods[pod.Name] = podInfo
	}

	return clusterState, nil
}