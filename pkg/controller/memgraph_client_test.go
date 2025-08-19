package controller

import (
	"context"
	"testing"
)

func TestMemgraphClient_QueryReplicationRole_EmptyAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)

	_, err := client.QueryReplicationRole(context.Background(), "")
	if err == nil {
		t.Error("Expected error for empty bolt address, got nil")
	}

	expectedMsg := "bolt address is empty"
	if err.Error() != expectedMsg {
		t.Errorf("Error message = %s, want %s", err.Error(), expectedMsg)
	}
}

func TestMemgraphClient_TestConnection_EmptyAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)

	err := client.TestConnection(context.Background(), "")
	if err == nil {
		t.Error("Expected error for empty bolt address, got nil")
	}

	expectedMsg := "bolt address is empty"
	if err.Error() != expectedMsg {
		t.Errorf("Error message = %s, want %s", err.Error(), expectedMsg)
	}
}

func TestMemgraphClient_QueryReplicationRole_InvalidAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)

	// Use an invalid address that will fail to connect
	_, err := client.QueryReplicationRole(context.Background(), "invalid-host:7687")
	if err == nil {
		t.Error("Expected error for invalid address, got nil")
	}

	// Should contain "failed to create driver" or "failed to verify connectivity"
	errMsg := err.Error()
	if errMsg == "" {
		t.Error("Error message is empty")
	}
}

func TestMemgraphClient_TestConnection_InvalidAddress(t *testing.T) {
	config := &Config{}
	client := NewMemgraphClient(config)

	// Use an invalid address that will fail to connect
	err := client.TestConnection(context.Background(), "invalid-host:7687")
	if err == nil {
		t.Error("Expected error for invalid address, got nil")
	}

	// Should contain connection failure message
	errMsg := err.Error()
	if errMsg == "" {
		t.Error("Error message is empty")
	}
}

func TestNewMemgraphClient(t *testing.T) {
	config := &Config{
		BoltPort: "7687",
	}

	client := NewMemgraphClient(config)
	if client == nil {
		t.Fatal("NewMemgraphClient() returned nil")
	}

	if client.config != config {
		t.Error("NewMemgraphClient() did not store config correctly")
	}
}

// Note: Integration tests with real Memgraph instances would go in e2e tests
// These unit tests focus on error handling and basic functionality