package controller

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	StateConfigMapName = "memgraph-controller-state"
)

// ControllerState represents the persistent state stored in ConfigMap
type ControllerState struct {
	MasterIndex        int       `json:"masterIndex"`
	LastUpdated        time.Time `json:"lastUpdated"`
	ControllerVersion  string    `json:"controllerVersion"`
	BootstrapCompleted bool      `json:"bootstrapCompleted"`
}

// StateManager manages controller state persistence using ConfigMaps
type StateManager struct {
	clientset kubernetes.Interface
	namespace string
}

// NewStateManager creates a new state manager
func NewStateManager(clientset kubernetes.Interface, namespace string) *StateManager {
	return &StateManager{
		clientset: clientset,
		namespace: namespace,
	}
}

// ConfigMapName returns the name of the ConfigMap used for state storage
func (sm *StateManager) ConfigMapName() string {
	return StateConfigMapName
}

// LoadState loads the controller state from ConfigMap
func (sm *StateManager) LoadState(ctx context.Context) (*ControllerState, error) {
	log.Printf("Loading controller state from ConfigMap %s/%s", sm.namespace, StateConfigMapName)

	configMap, err := sm.clientset.CoreV1().ConfigMaps(sm.namespace).Get(
		ctx, StateConfigMapName, metav1.GetOptions{})
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

	// Parse lastUpdated
	lastUpdatedStr, exists := configMap.Data["lastUpdated"]
	if !exists {
		return nil, fmt.Errorf("lastUpdated not found in ConfigMap")
	}

	lastUpdated, err := time.Parse(time.RFC3339, lastUpdatedStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse lastUpdated: %w", err)
	}

	// Parse controllerVersion (optional)
	controllerVersion := configMap.Data["controllerVersion"]

	// Parse bootstrapCompleted (optional, default to true if missing)
	bootstrapCompleted := true
	if bootstrapCompletedStr, exists := configMap.Data["bootstrapCompleted"]; exists {
		if parsed, err := strconv.ParseBool(bootstrapCompletedStr); err == nil {
			bootstrapCompleted = parsed
		}
	}

	state := &ControllerState{
		MasterIndex:        masterIndex,
		LastUpdated:        lastUpdated,
		ControllerVersion:  controllerVersion,
		BootstrapCompleted: bootstrapCompleted,
	}

	log.Printf("Loaded controller state: masterIndex=%d, lastUpdated=%s, bootstrapCompleted=%t",
		state.MasterIndex, state.LastUpdated.Format(time.RFC3339), state.BootstrapCompleted)

	return state, nil
}

// SaveState persists the controller state to ConfigMap
func (sm *StateManager) SaveState(ctx context.Context, state *ControllerState) error {
	log.Printf("Saving controller state: masterIndex=%d, bootstrapCompleted=%t",
		state.MasterIndex, state.BootstrapCompleted)

	// Create ConfigMap data
	data := map[string]string{
		"masterIndex":        strconv.Itoa(state.MasterIndex),
		"lastUpdated":        state.LastUpdated.Format(time.RFC3339),
		"controllerVersion":  state.ControllerVersion,
		"bootstrapCompleted": strconv.FormatBool(state.BootstrapCompleted),
	}

	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      StateConfigMapName,
			Namespace: sm.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "memgraph-controller",
				"app.kubernetes.io/component": "state",
			},
		},
		Data: data,
	}

	// Try to update existing ConfigMap first
	existingConfigMap, err := sm.clientset.CoreV1().ConfigMaps(sm.namespace).Get(
		ctx, StateConfigMapName, metav1.GetOptions{})
	if err == nil {
		// Update existing ConfigMap
		existingConfigMap.Data = data
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
		log.Printf("Created new state ConfigMap")
	}

	return nil
}

// StateExists checks if the state ConfigMap exists
func (sm *StateManager) StateExists(ctx context.Context) (bool, error) {
	_, err := sm.clientset.CoreV1().ConfigMaps(sm.namespace).Get(
		ctx, StateConfigMapName, metav1.GetOptions{})
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
		ctx, StateConfigMapName, metav1.DeleteOptions{})
	if err != nil && !IsNotFoundError(err) {
		return fmt.Errorf("failed to delete state ConfigMap: %w", err)
	}
	log.Printf("Deleted state ConfigMap")
	return nil
}

// GetStateVersion returns the controller version from the stored state
func (sm *StateManager) GetStateVersion(ctx context.Context) (string, error) {
	state, err := sm.LoadState(ctx)
	if err != nil {
		return "", err
	}
	return state.ControllerVersion, nil
}

// IsNotFoundError checks if the error is a Kubernetes not found error
func IsNotFoundError(err error) bool {
	return errors.IsNotFound(err)
}