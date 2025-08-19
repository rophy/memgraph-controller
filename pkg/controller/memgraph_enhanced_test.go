package controller

import (
	"context"
	"testing"
	"time"
)

func TestMemgraphClient_QueryReplicasWithRetry_EmptyAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)
	defer client.Close(context.Background())

	_, err := client.QueryReplicasWithRetry(context.Background(), "")
	if err == nil {
		t.Error("Expected error for empty bolt address, got nil")
	}

	expectedMsg := "bolt address is empty"
	if err.Error() != expectedMsg {
		t.Errorf("Error message = %s, want %s", err.Error(), expectedMsg)
	}
}

func TestMemgraphClient_QueryReplicationRoleWithRetry_EmptyAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)
	defer client.Close(context.Background())

	_, err := client.QueryReplicationRoleWithRetry(context.Background(), "")
	if err == nil {
		t.Error("Expected error for empty bolt address, got nil")
	}

	expectedMsg := "bolt address is empty"
	if err.Error() != expectedMsg {
		t.Errorf("Error message = %s, want %s", err.Error(), expectedMsg)
	}
}

func TestMemgraphClient_TestConnectionWithRetry_EmptyAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)
	defer client.Close(context.Background())

	err := client.TestConnectionWithRetry(context.Background(), "")
	if err == nil {
		t.Error("Expected error for empty bolt address, got nil")
	}

	expectedMsg := "bolt address is empty"
	if err.Error() != expectedMsg {
		t.Errorf("Error message = %s, want %s", err.Error(), expectedMsg)
	}
}

func TestMemgraphClient_QueryReplicasWithRetry_InvalidAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)
	defer client.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Use an invalid address that will fail to connect
	_, err := client.QueryReplicasWithRetry(ctx, "invalid-host:7687")
	if err == nil {
		t.Error("Expected error for invalid address, got nil")
	}

	// Should contain retry failure message
	errMsg := err.Error()
	if errMsg == "" {
		t.Error("Error message is empty")
	}
	
	// Should mention retries in the error
	if !contains(errMsg, "after retries") {
		t.Errorf("Expected error to mention retries, got: %s", errMsg)
	}
}

func TestMemgraphClient_QueryReplicationRoleWithRetry_InvalidAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)
	defer client.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Use an invalid address that will fail to connect
	_, err := client.QueryReplicationRoleWithRetry(ctx, "invalid-host:7687")
	if err == nil {
		t.Error("Expected error for invalid address, got nil")
	}

	// Should contain retry failure message
	errMsg := err.Error()
	if errMsg == "" {
		t.Error("Error message is empty")
	}
	
	// Should mention retries in the error
	if !contains(errMsg, "after retries") {
		t.Errorf("Expected error to mention retries, got: %s", errMsg)
	}
}

func TestMemgraphClient_TestConnectionWithRetry_InvalidAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)
	defer client.Close(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Use an invalid address that will fail to connect
	err := client.TestConnectionWithRetry(ctx, "invalid-host:7687")
	if err == nil {
		t.Error("Expected error for invalid address, got nil")
	}

	// Should contain retry failure message
	errMsg := err.Error()
	if errMsg == "" {
		t.Error("Error message is empty")
	}
	
	// Should mention retries in the error
	if !contains(errMsg, "after retries") {
		t.Errorf("Expected error to mention retries, got: %s", errMsg)
	}
}

func TestNewMemgraphClient_WithRetryConfig(t *testing.T) {
	config := &Config{
		BoltPort: "7687",
	}

	client := NewMemgraphClient(config)
	defer client.Close(context.Background())

	if client == nil {
		t.Fatal("NewMemgraphClient() returned nil")
	}

	if client.config != config {
		t.Error("NewMemgraphClient() did not store config correctly")
	}

	if client.connectionPool == nil {
		t.Error("NewMemgraphClient() did not create connection pool")
	}

	// Test default retry configuration
	if client.retryConfig.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", client.retryConfig.MaxRetries)
	}

	if client.retryConfig.BaseDelay != 1*time.Second {
		t.Errorf("BaseDelay = %v, want 1s", client.retryConfig.BaseDelay)
	}

	if client.retryConfig.MaxDelay != 10*time.Second {
		t.Errorf("MaxDelay = %v, want 10s", client.retryConfig.MaxDelay)
	}
}

func TestReplicaInfo_Structure(t *testing.T) {
	replica := ReplicaInfo{
		Name:            "test-replica",
		SocketAddress:   "10.0.0.1:10000",
		SyncMode:        "SYNC",
		SystemTimestamp: 1234567890,
		CheckFrequency:  5000,
	}

	if replica.Name != "test-replica" {
		t.Errorf("Name = %s, want test-replica", replica.Name)
	}

	if replica.SocketAddress != "10.0.0.1:10000" {
		t.Errorf("SocketAddress = %s, want 10.0.0.1:10000", replica.SocketAddress)
	}

	if replica.SyncMode != "SYNC" {
		t.Errorf("SyncMode = %s, want SYNC", replica.SyncMode)
	}

	if replica.SystemTimestamp != 1234567890 {
		t.Errorf("SystemTimestamp = %d, want 1234567890", replica.SystemTimestamp)
	}

	if replica.CheckFrequency != 5000 {
		t.Errorf("CheckFrequency = %d, want 5000", replica.CheckFrequency)
	}
}

func TestReplicasResponse_Structure(t *testing.T) {
	replicas := []ReplicaInfo{
		{Name: "replica1", SocketAddress: "10.0.0.1:10000"},
		{Name: "replica2", SocketAddress: "10.0.0.2:10000"},
	}

	response := ReplicasResponse{Replicas: replicas}

	if len(response.Replicas) != 2 {
		t.Errorf("Replicas length = %d, want 2", len(response.Replicas))
	}

	if response.Replicas[0].Name != "replica1" {
		t.Errorf("First replica name = %s, want replica1", response.Replicas[0].Name)
	}

	if response.Replicas[1].Name != "replica2" {
		t.Errorf("Second replica name = %s, want replica2", response.Replicas[1].Name)
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || 
		(len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || 
		func() bool {
			for i := 1; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())))
}