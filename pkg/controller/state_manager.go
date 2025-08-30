package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ControllerState represents the persistent state stored in ConfigMap
type ControllerState struct {
	MasterIndex int `json:"masterIndex"`
}

// StateManager manages controller state persistence using ConfigMaps
type StateManager struct {
	clientset     kubernetes.Interface
	namespace     string
	configMapName string
}

// NewStateManager creates a new state manager
func NewStateManager(clientset kubernetes.Interface, namespace string, configMapName string) *StateManager {
	return &StateManager{
		clientset:     clientset,
		namespace:     namespace,
		configMapName: configMapName,
	}
}

// ConfigMapName returns the name of the ConfigMap used for state storage
func (sm *StateManager) ConfigMapName() string {
	return sm.configMapName
}

// LoadState loads the controller state from ConfigMap
func (sm *StateManager) LoadState(ctx context.Context) (*ControllerState, error) {
	log.Printf("Loading controller state from ConfigMap %s/%s", sm.namespace, sm.configMapName)

	configMap, err := sm.clientset.CoreV1().ConfigMaps(sm.namespace).Get(
		ctx, sm.configMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get state ConfigMap: %w", err)
	}

	// Parse masterIndex
	masterIndexStr, exists := configMap.Data["masterIndex"]
	if !exists {
		return nil, fmt.Errorf("masterIndex not found in ConfigMap")
	}

	masterIndex, err := strconv.Atoi(masterIndexStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse masterIndex: %w", err)
	}

	state := &ControllerState{
		MasterIndex: masterIndex,
	}

	log.Printf("Loaded controller state: masterIndex=%d", state.MasterIndex)

	return state, nil
}

// SaveState persists the controller state to ConfigMap
func (sm *StateManager) SaveState(ctx context.Context, state *ControllerState) error {
	log.Printf("Saving controller state: masterIndex=%d", state.MasterIndex)

	// Create ConfigMap data
	data := map[string]string{
		"masterIndex": strconv.Itoa(state.MasterIndex),
	}

	// Get owner reference for the controller deployment
	ownerRef, err := sm.getControllerDeploymentOwnerRef(ctx)
	if err != nil {
		log.Printf("Warning: Failed to get controller deployment owner reference: %v", err)
		// Continue without owner reference rather than failing
	}

	// Prepare owner references slice
	var ownerRefs []metav1.OwnerReference
	if ownerRef != nil {
		ownerRefs = []metav1.OwnerReference{*ownerRef}
	}

	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sm.configMapName,
			Namespace: sm.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "memgraph-controller",
				"app.kubernetes.io/component": "state",
			},
			OwnerReferences: ownerRefs,
		},
		Data: data,
	}

	// Try to update existing ConfigMap first
	existingConfigMap, err := sm.clientset.CoreV1().ConfigMaps(sm.namespace).Get(
		ctx, sm.configMapName, metav1.GetOptions{})
	if err == nil {
		// Update existing ConfigMap with new data and owner reference
		existingConfigMap.Data = data
		// Add owner reference if we have one and it's not already set
		if ownerRef != nil {
			hasOwner := false
			for _, existing := range existingConfigMap.OwnerReferences {
				if existing.UID == ownerRef.UID {
					hasOwner = true
					break
				}
			}
			if !hasOwner {
				existingConfigMap.OwnerReferences = ownerRefs
				log.Printf("Added owner reference to existing ConfigMap")
			}
		}
		_, err = sm.clientset.CoreV1().ConfigMaps(sm.namespace).Update(ctx, existingConfigMap, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update state ConfigMap: %w", err)
		}
		log.Printf("Updated existing state ConfigMap")
	} else {
		// Create new ConfigMap
		_, err = sm.clientset.CoreV1().ConfigMaps(sm.namespace).Create(ctx, configMap, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("failed to create state ConfigMap: %w", err)
		}
		if ownerRef != nil {
			log.Printf("Created new state ConfigMap with owner reference to Deployment %s", ownerRef.Name)
		} else {
			log.Printf("Created new state ConfigMap without owner reference")
		}
	}

	return nil
}

// StateExists checks if the state ConfigMap exists
func (sm *StateManager) StateExists(ctx context.Context) (bool, error) {
	_, err := sm.clientset.CoreV1().ConfigMaps(sm.namespace).Get(
		ctx, sm.configMapName, metav1.GetOptions{})
	if err != nil {
		// Check if it's a not found error
		if IsNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check state ConfigMap existence: %w", err)
	}
	return true, nil
}

// DeleteState removes the state ConfigMap (useful for testing)
func (sm *StateManager) DeleteState(ctx context.Context) error {
	err := sm.clientset.CoreV1().ConfigMaps(sm.namespace).Delete(
		ctx, sm.configMapName, metav1.DeleteOptions{})
	if err != nil && !IsNotFoundError(err) {
		return fmt.Errorf("failed to delete state ConfigMap: %w", err)
	}
	log.Printf("Deleted state ConfigMap")
	return nil
}


// getControllerDeploymentOwnerRef gets the owner reference for the controller deployment
func (sm *StateManager) getControllerDeploymentOwnerRef(ctx context.Context) (*metav1.OwnerReference, error) {
	// Get pod name from environment variable (set by Kubernetes via fieldRef)
	podName := os.Getenv("POD_NAME")
	if podName == "" {
		log.Printf("Warning: POD_NAME env var not set, ConfigMap will not have owner reference")
		return nil, nil
	}

	// Get the pod to find its owner (the ReplicaSet)
	pod, err := sm.clientset.CoreV1().Pods(sm.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s: %w", podName, err)
	}

	// Find the ReplicaSet owner of the pod
	var replicaSetRef *metav1.OwnerReference
	for _, ownerRef := range pod.OwnerReferences {
		if ownerRef.Kind == "ReplicaSet" && ownerRef.APIVersion == "apps/v1" {
			replicaSetRef = &ownerRef
			break
		}
	}

	if replicaSetRef == nil {
		log.Printf("Warning: Pod %s does not have a ReplicaSet owner, ConfigMap will not have owner reference", podName)
		return nil, nil
	}

	// Get the ReplicaSet to find its owner (the Deployment)
	replicaSet, err := sm.clientset.AppsV1().ReplicaSets(sm.namespace).Get(ctx, replicaSetRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get ReplicaSet %s: %w", replicaSetRef.Name, err)
	}

	// Find the Deployment owner of the ReplicaSet
	var deploymentRef *metav1.OwnerReference
	for _, ownerRef := range replicaSet.OwnerReferences {
		if ownerRef.Kind == "Deployment" && ownerRef.APIVersion == "apps/v1" {
			deploymentRef = &ownerRef
			break
		}
	}

	if deploymentRef == nil {
		log.Printf("Warning: ReplicaSet %s does not have a Deployment owner, ConfigMap will not have owner reference", replicaSetRef.Name)
		return nil, nil
	}

	log.Printf("Found controller Deployment owner: %s", deploymentRef.Name)
	return deploymentRef, nil
}

// IsNotFoundError checks if the error is a Kubernetes not found error
func IsNotFoundError(err error) bool {
	return errors.IsNotFound(err)
}