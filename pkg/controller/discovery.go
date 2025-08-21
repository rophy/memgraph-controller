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
		LabelSelector: "app.kubernetes.io/name=" + pd.config.AppName,
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

		log.Printf("Discovered pod: %s, IP: %s, Timestamp: %s",
			podInfo.Name,
			pod.Status.PodIP,
			podInfo.Timestamp.Format(time.RFC3339))
	}

	// Master selection will be done AFTER querying actual Memgraph state
	// This ensures we use real replication roles for decision making
	log.Printf("Pod discovery complete. Master selection deferred until after Memgraph querying.")

	return clusterState, nil
}

func (pd *PodDiscovery) selectMaster(clusterState *ClusterState) {
	// SYNC Replica Priority Strategy:
	// 1. Look for existing MAIN node (prefer current master if healthy)
	// 2. If no MAIN, look for SYNC replica (guaranteed consistency)
	// 3. If no SYNC replica, fall back to timestamp-based selection
	
	var currentMain *PodInfo
	var syncReplica *PodInfo
	var latestPod *PodInfo
	var latestTime time.Time

	// Analyze all pods for master selection
	for _, podInfo := range clusterState.Pods {
		// Priority 1: Existing MAIN node (prefer current master)
		if podInfo.MemgraphRole == "main" {
			currentMain = podInfo
			log.Printf("Found existing MAIN node: %s", podInfo.Name)
		}
		
		// Priority 2: SYNC replica (guaranteed data consistency)
		if podInfo.IsSyncReplica {
			syncReplica = podInfo
			log.Printf("Found SYNC replica: %s", podInfo.Name)
		}
		
		// Priority 3: Latest timestamp (fallback)
		if latestPod == nil || podInfo.Timestamp.After(latestTime) {
			latestPod = podInfo
			latestTime = podInfo.Timestamp
		}
	}

	// Master selection decision tree
	var selectedMaster *PodInfo
	var selectionReason string
	
	if currentMain != nil {
		// Prefer existing MAIN node (avoid unnecessary failover)
		selectedMaster = currentMain
		selectionReason = "existing MAIN node"
	} else if syncReplica != nil {
		// ONLY safe automatic promotion: SYNC replica has all committed data
		selectedMaster = syncReplica
		selectionReason = "SYNC replica (guaranteed consistency)"
		log.Printf("PROMOTING SYNC REPLICA: %s has all committed transactions", syncReplica.Name)
	} else {
		// CRITICAL: No SYNC replica available - DO NOT auto-promote ASYNC replicas
		// ASYNC replicas may be missing committed transactions, causing data loss
		if len(clusterState.Pods) > 1 {
			log.Printf("CRITICAL: No SYNC replica available for safe automatic promotion")
			log.Printf("CRITICAL: Cannot guarantee data consistency - manual intervention required")
			log.Printf("CRITICAL: ASYNC replicas may be missing committed transactions")
			
			// Do NOT select any master - require manual intervention
			selectedMaster = nil
			selectionReason = "no safe automatic promotion possible (SYNC replica unavailable)"
		} else {
			// Single pod scenario - safe to promote (no replication risk)
			selectedMaster = latestPod
			selectionReason = "single pod cluster (no replication consistency risk)"
		}
	}

	if selectedMaster != nil {
		clusterState.CurrentMaster = selectedMaster.Name
		log.Printf("Selected master: %s (reason: %s, timestamp: %s)",
			selectedMaster.Name,
			selectionReason,
			selectedMaster.Timestamp.Format(time.RFC3339))
	} else {
		log.Printf("No pods available for master selection")
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

