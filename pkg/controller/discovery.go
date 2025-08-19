package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// UpdatePodLabel updates a specific label on a pod
func (pd *PodDiscovery) UpdatePodLabel(ctx context.Context, podName, labelKey, labelValue string) error {
	if podName == "" {
		return fmt.Errorf("pod name cannot be empty")
	}
	if labelKey == "" {
		return fmt.Errorf("label key cannot be empty")
	}

	// First, check if the pod exists and has labels
	pod, err := pd.clientset.CoreV1().Pods(pd.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod %s: %w", podName, err)
	}

	var patchData string
	if pod.Labels == nil {
		// Need to create the entire labels map
		patchData = fmt.Sprintf(`[{"op":"add","path":"/metadata/labels","value":{"%s":"%s"}}]`, labelKey, labelValue)
	} else {
		// Labels map exists, just add/update the specific label
		patchData = fmt.Sprintf(`[{"op":"add","path":"/metadata/labels/%s","value":"%s"}]`, labelKey, labelValue)
	}
	
	_, err = pd.clientset.CoreV1().Pods(pd.config.Namespace).Patch(
		ctx,
		podName,
		types.JSONPatchType,
		[]byte(patchData),
		metav1.PatchOptions{},
	)
	
	if err != nil {
		return fmt.Errorf("failed to update label %s=%s on pod %s: %w", labelKey, labelValue, podName, err)
	}

	log.Printf("Successfully updated label %s=%s on pod %s", labelKey, labelValue, podName)
	return nil
}

// UpdatePodRoleLabel updates the role label on a pod with validation
func (pd *PodDiscovery) UpdatePodRoleLabel(ctx context.Context, podName string, role PodState) error {
	if podName == "" {
		return fmt.Errorf("pod name cannot be empty")
	}

	var roleValue string
	switch role {
	case MASTER:
		roleValue = "master"
	case REPLICA:
		roleValue = "replica"
	case INITIAL:
		// For INITIAL state, we remove the role label
		return pd.RemovePodLabel(ctx, podName, "role")
	default:
		return fmt.Errorf("invalid pod state for role label: %v", role)
	}

	return pd.UpdatePodLabel(ctx, podName, "role", roleValue)
}

// RemovePodLabel removes a specific label from a pod
func (pd *PodDiscovery) RemovePodLabel(ctx context.Context, podName, labelKey string) error {
	if podName == "" {
		return fmt.Errorf("pod name cannot be empty")
	}
	if labelKey == "" {
		return fmt.Errorf("label key cannot be empty")
	}

	// First, check if the pod exists and has the label
	pod, err := pd.clientset.CoreV1().Pods(pd.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod %s: %w", podName, err)
	}

	if pod.Labels == nil || pod.Labels[labelKey] == "" {
		log.Printf("Label %s does not exist on pod %s, nothing to remove", labelKey, podName)
		return nil
	}

	// Create JSON patch to remove the label
	patchData := fmt.Sprintf(`[{"op":"remove","path":"/metadata/labels/%s"}]`, labelKey)
	
	_, err = pd.clientset.CoreV1().Pods(pd.config.Namespace).Patch(
		ctx,
		podName,
		types.JSONPatchType,
		[]byte(patchData),
		metav1.PatchOptions{},
	)
	
	if err != nil {
		return fmt.Errorf("failed to remove label %s from pod %s: %w", labelKey, podName, err)
	}

	log.Printf("Successfully removed label %s from pod %s", labelKey, podName)
	return nil
}

// ValidatePodLabels checks if pod labels match the expected replication state
func (pd *PodDiscovery) ValidatePodLabels(ctx context.Context, clusterState *ClusterState) ([]LabelInconsistency, error) {
	var inconsistencies []LabelInconsistency

	for podName, podInfo := range clusterState.Pods {
		expectedRole := pd.getExpectedRoleLabel(podInfo, clusterState.CurrentMaster)
		currentRole := podInfo.KubernetesRole

		if currentRole != expectedRole {
			inconsistency := LabelInconsistency{
				PodName:      podName,
				CurrentLabel: currentRole,
				ExpectedLabel: expectedRole,
				CurrentState: podInfo.State,
				MemgraphRole: podInfo.MemgraphRole,
			}
			inconsistencies = append(inconsistencies, inconsistency)
		}
	}

	return inconsistencies, nil
}

// getExpectedRoleLabel determines what the role label should be based on pod state
func (pd *PodDiscovery) getExpectedRoleLabel(podInfo *PodInfo, currentMaster string) string {
	switch podInfo.State {
	case MASTER:
		return "master"
	case REPLICA:
		return "replica"
	case INITIAL:
		return "" // No role label for INITIAL state
	default:
		return ""
	}
}

// BatchUpdatePodLabels updates role labels for multiple pods with rollback on failure
func (pd *PodDiscovery) BatchUpdatePodLabels(ctx context.Context, updates []PodLabelUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	log.Printf("Starting batch label update for %d pods", len(updates))

	// Track successful updates for potential rollback
	var successful []PodLabelUpdate
	var failed []error

	// Apply updates one by one
	for _, update := range updates {
		var err error
		
		if update.Remove {
			err = pd.RemovePodLabel(ctx, update.PodName, "role")
		} else {
			err = pd.UpdatePodLabel(ctx, update.PodName, "role", update.RoleValue)
		}

		if err != nil {
			log.Printf("Failed to update label for pod %s: %v", update.PodName, err)
			failed = append(failed, fmt.Errorf("pod %s: %w", update.PodName, err))
			
			// Rollback successful updates
			if len(successful) > 0 {
				log.Printf("Rolling back %d successful label updates due to failure", len(successful))
				pd.rollbackLabelUpdates(ctx, successful)
			}
			
			return fmt.Errorf("batch label update failed after %d successful updates: %v", len(successful), failed)
		}

		successful = append(successful, update)
		log.Printf("Successfully updated label for pod %s", update.PodName)
	}

	log.Printf("Batch label update completed successfully for %d pods", len(successful))
	return nil
}

// rollbackLabelUpdates attempts to rollback a set of label updates
func (pd *PodDiscovery) rollbackLabelUpdates(ctx context.Context, updates []PodLabelUpdate) {
	for _, update := range updates {
		var err error
		
		// Reverse the operation
		if update.Remove {
			// If we removed a label, try to restore it with the original value
			if update.OriginalValue != "" {
				err = pd.UpdatePodLabel(ctx, update.PodName, "role", update.OriginalValue)
			}
		} else {
			// If we added/updated a label, remove it or restore original
			if update.OriginalValue == "" {
				err = pd.RemovePodLabel(ctx, update.PodName, "role")
			} else {
				err = pd.UpdatePodLabel(ctx, update.PodName, "role", update.OriginalValue)
			}
		}

		if err != nil {
			log.Printf("WARNING: Failed to rollback label update for pod %s: %v", update.PodName, err)
		} else {
			log.Printf("Rolled back label update for pod %s", update.PodName)
		}
	}
}

// SyncPodLabelsWithState synchronizes pod labels with their current replication state
func (pd *PodDiscovery) SyncPodLabelsWithState(ctx context.Context, clusterState *ClusterState) error {
	// Validate current labels and identify inconsistencies
	inconsistencies, err := pd.ValidatePodLabels(ctx, clusterState)
	if err != nil {
		return fmt.Errorf("failed to validate pod labels: %w", err)
	}

	if len(inconsistencies) == 0 {
		log.Println("All pod labels are consistent with replication state")
		return nil
	}

	log.Printf("Found %d label inconsistencies, preparing batch update", len(inconsistencies))

	// Prepare batch updates to fix inconsistencies
	var updates []PodLabelUpdate
	for _, inconsistency := range inconsistencies {
		update := PodLabelUpdate{
			PodName:       inconsistency.PodName,
			RoleValue:     inconsistency.ExpectedLabel,
			OriginalValue: inconsistency.CurrentLabel,
			Remove:        inconsistency.ExpectedLabel == "",
		}
		updates = append(updates, update)
		
		log.Printf("Pod %s: current=%s, expected=%s, action=%s", 
			inconsistency.PodName, 
			inconsistency.CurrentLabel, 
			inconsistency.ExpectedLabel,
			map[bool]string{true: "remove", false: "update"}[update.Remove])
	}

	// Apply batch updates with rollback on failure
	return pd.BatchUpdatePodLabels(ctx, updates)
}

// LabelInconsistency represents a mismatch between pod labels and replication state
type LabelInconsistency struct {
	PodName       string
	CurrentLabel  string
	ExpectedLabel string
	CurrentState  PodState
	MemgraphRole  string
}

// PodLabelUpdate represents a label update operation
type PodLabelUpdate struct {
	PodName       string
	RoleValue     string
	OriginalValue string
	Remove        bool
}