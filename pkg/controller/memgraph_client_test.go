package controller

import "memgraph-controller/pkg/common"

import (
	"context"
	"testing"
)

func TestMemgraphClient_QueryReplicationRole_EmptyAddress(t *testing.T) {
	config := &common.Config{}
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
	config := &common.Config{}
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


func TestNewMemgraphClient(t *testing.T) {
	config := &common.Config{
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