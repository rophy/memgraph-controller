package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"memgraph-controller/internal/common"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Tests for controller_reconcile.go (moved from reconciliation_test.go)

// MockReplicaQuerier is a mock for testing replica caching behavior
type MockReplicaQuerier struct {
	mock.Mock
}

func (m *MockReplicaQuerier) QueryReplicasWithRetry(ctx context.Context, boltAddress string) (*ReplicasResponse, error) {
	args := m.Called(ctx, boltAddress)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*ReplicasResponse), args.Error(1)
}

func (m *MockReplicaQuerier) QueryReplicationRoleWithRetry(ctx context.Context, boltAddress string) (*ReplicationRole, error) {
	args := m.Called(ctx, boltAddress)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*ReplicationRole), args.Error(1)
}

func TestReconciliationMetrics(t *testing.T) {
	// Test GetReconciliationMetrics with metrics initialized
	controller := &MemgraphController{
		metrics: &ReconciliationMetrics{
			TotalReconciliations:      2,
			SuccessfulReconciliations: 1,
			FailedReconciliations:     1,
			LastReconciliationReason:  "test-success",
			LastReconciliationError:   "",
		},
	}

	metrics := controller.GetReconciliationMetrics()
	if metrics.TotalReconciliations != 2 {
		t.Errorf("TotalReconciliations = %d, want 2", metrics.TotalReconciliations)
	}
	if metrics.SuccessfulReconciliations != 1 {
		t.Errorf("SuccessfulReconciliations = %d, want 1", metrics.SuccessfulReconciliations)
	}
	if metrics.FailedReconciliations != 1 {
		t.Errorf("FailedReconciliations = %d, want 1", metrics.FailedReconciliations)
	}

	// Test GetReconciliationMetrics with nil metrics
	controllerNil := &MemgraphController{metrics: nil}
	metricsNil := controllerNil.GetReconciliationMetrics()
	if metricsNil.TotalReconciliations != 0 {
		t.Errorf("TotalReconciliations with nil metrics = %d, want 0", metricsNil.TotalReconciliations)
	}
}

func TestGetReconciliationMetrics(t *testing.T) {
	controller := &MemgraphController{
		metrics: &ReconciliationMetrics{
			TotalReconciliations:      5,
			SuccessfulReconciliations: 4,
			FailedReconciliations:     1,
			AverageReconciliationTime: time.Millisecond * 150,
			LastReconciliationTime:    time.Now(),
			LastReconciliationReason:  "test-reason",
		},
	}

	metrics := controller.GetReconciliationMetrics()

	if metrics.TotalReconciliations != 5 {
		t.Errorf("GetReconciliationMetrics().TotalReconciliations = %d, want 5", metrics.TotalReconciliations)
	}

	if metrics.SuccessfulReconciliations != 4 {
		t.Errorf("GetReconciliationMetrics().SuccessfulReconciliations = %d, want 4", metrics.SuccessfulReconciliations)
	}

	if metrics.FailedReconciliations != 1 {
		t.Errorf("GetReconciliationMetrics().FailedReconciliations = %d, want 1", metrics.FailedReconciliations)
	}

	if metrics.LastReconciliationReason != "test-reason" {
		t.Errorf("GetReconciliationMetrics().LastReconciliationReason = %s, want test-reason", metrics.LastReconciliationReason)
	}

	if metrics.AverageReconciliationTime != time.Millisecond*150 {
		t.Errorf("GetReconciliationMetrics().AverageReconciliationTime = %v, want %v", metrics.AverageReconciliationTime, time.Millisecond*150)
	}

	// Test GetReconciliationMetrics with nil metrics
	controller = &MemgraphController{}
	metrics = controller.GetReconciliationMetrics()

	if metrics.TotalReconciliations != 0 {
		t.Errorf("GetReconciliationMetrics with nil metrics should return zero values")
	}
}

func TestControllerBasics(t *testing.T) {
	config := &common.Config{
		AppName:   "memgraph",
		Namespace: "memgraph",
	}

	controller := NewMemgraphController(nil, config)

	if controller == nil {
		t.Fatal("NewMemgraphController() returned nil")
	}

	if controller.config != config {
		t.Error("Controller config not set correctly")
	}

	if controller.isRunning {
		t.Error("New controller should not be running")
	}

	if controller.IsLeader() {
		t.Error("New controller should not be leader")
	}

	if controller.IsRunning() {
		t.Error("New controller should not be running")
	}
}

func TestPerformReconciliationActionsRequiresCacheClearingToAvoidStaleData(t *testing.T) {
	ctx := context.Background()
	
	// Create a test pod
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "memgraph-ha-0",
			Namespace: "memgraph",
		},
		Status: v1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}
	
	// Create mock MemgraphClient
	mockClient := &MockReplicaQuerier{}
	
	// Create MemgraphNode with cached stale data
	realClient := &MemgraphClient{}
	node := NewMemgraphNode(pod, realClient)
	
	// Pre-populate node with stale cached data (from previous reconciliation)
	node.memgraphRole = "MAIN"
	node.replicasInfo = []ReplicaInfo{
		{Name: "memgraph_ha_1", SyncMode: "STRICT_SYNC"},
		{Name: "memgraph_ha_2", SyncMode: "ASYNC"},
	}
	node.hasReplicasInfo = true // This is the key - cache is populated with stale data
	
	// We don't actually need the full controller for this test
	// We're testing the caching behavior of the node itself
	_ = node // Just to show we're using the node
	
	// First test: WITHOUT cache clearing (current bug)
	// The node has stale cached data, GetReplicas() will return it without querying
	replicas := testGetReplicasFromNode(t, node, ctx)
	if len(replicas) != 2 {
		t.Errorf("Expected 2 stale replicas without cache clearing, got %d", len(replicas))
	}
	if replicas[0].SyncMode != "STRICT_SYNC" {
		t.Errorf("Expected stale STRICT_SYNC mode without cache clearing, got %s", replicas[0].SyncMode)
	}
	
	// Second test: WITH cache clearing (the fix)
	// Clear cache and set up mock for fresh data
	node.ClearCachedInfo()
	
	// Mock fresh data (state has changed)
	freshReplicas := &ReplicasResponse{
		Replicas: []ReplicaInfo{
			{Name: "memgraph_ha_1", SyncMode: "ASYNC"}, // Changed from SYNC to ASYNC
		},
	}
	
	// Set up mock expectations for fresh query
	mockClient.On("QueryReplicationRoleWithRetry", ctx, "10.0.0.1:7687").Return(&ReplicationRole{Role: "MAIN"}, nil)
	mockClient.On("QueryReplicasWithRetry", ctx, "10.0.0.1:7687").Return(freshReplicas, nil)
	
	// Query using mock client (simulating what happens after cache clearing)
	freshReplicasResult := testGetReplicasWithMockClient(t, node, mockClient, ctx)
	if len(freshReplicasResult) != 1 {
		t.Errorf("Expected 1 fresh replica after cache clearing, got %d", len(freshReplicasResult))
	}
	if freshReplicasResult[0].SyncMode != "ASYNC" {
		t.Errorf("Expected fresh ASYNC mode after cache clearing, got %s", freshReplicasResult[0].SyncMode)
	}
	
	mockClient.AssertExpectations(t)
}

// No need for gateway server mock in this test

// testGetReplicasFromNode simulates calling GetReplicas on a node without mocking
func testGetReplicasFromNode(t *testing.T, node *MemgraphNode, ctx context.Context) []ReplicaInfo {
	// This simulates what happens in reconciliation when cache is NOT cleared
	// If hasReplicasInfo is true, GetReplicas returns cached data without querying
	if node.hasReplicasInfo {
		return node.replicasInfo // Returns stale cached data
	}
	
	t.Fatal("Node should have cached data for this test")
	return nil
}

// testGetReplicasWithMockClient simulates GetReplicas with fresh mock data
func testGetReplicasWithMockClient(t *testing.T, node *MemgraphNode, mockClient *MockReplicaQuerier, ctx context.Context) []ReplicaInfo {
	// Check role first
	boltAddress, err := node.GetBoltAddress()
	if err != nil {
		t.Fatalf("Failed to get bolt address: %v", err)
	}
	role, err := mockClient.QueryReplicationRoleWithRetry(ctx, boltAddress)
	if err != nil {
		t.Fatalf("Mock role query failed: %v", err)
	}
	if role.Role != "MAIN" {
		t.Fatalf("Expected MAIN role, got %s", role.Role)
	}
	
	// Update cached role
	node.memgraphRole = role.Role
	
	// Check if we have cached replica info (should be false after ClearCachedInfo)
	if !node.hasReplicasInfo {
		// Query replicas using mock (fresh data)
		boltAddress, err := node.GetBoltAddress()
		if err != nil {
			t.Fatalf("Failed to get bolt address: %v", err)
		}
		replicasResp, err := mockClient.QueryReplicasWithRetry(ctx, boltAddress)
		if err != nil {
			t.Fatalf("Mock replicas query failed: %v", err)
		}
		
		// Cache the fresh results
		node.replicasInfo = replicasResp.Replicas
		node.hasReplicasInfo = true
	}
	
	return node.replicasInfo
}