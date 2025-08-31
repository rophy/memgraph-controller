package controller

import (
	"context"
	"fmt"
)

// MockStateManager for testing - implements StateManagerInterface
type MockStateManager struct {
	state  *ControllerState
	err    error
	exists bool
}

func NewMockStateManager(masterIndex int) *MockStateManager {
	return &MockStateManager{
		state: &ControllerState{
			TargetMainIndex: masterIndex,
		},
		exists: true,
	}
}

func NewEmptyMockStateManager() *MockStateManager {
	return &MockStateManager{
		state:  nil,
		exists: false,
	}
}

func (m *MockStateManager) LoadState(ctx context.Context) (*ControllerState, error) {
	if m.err != nil {
		return nil, m.err
	}
	if !m.exists || m.state == nil {
		return nil, fmt.Errorf("state not found")
	}
	return m.state, nil
}

func (m *MockStateManager) SaveState(ctx context.Context, state *ControllerState) error {
	if m.err != nil {
		return m.err
	}
	m.state = state
	m.exists = true
	return nil
}

func (m *MockStateManager) StateExists(ctx context.Context) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	return m.exists, nil
}

func (m *MockStateManager) ConfigMapName() string {
	return "test-configmap"
}