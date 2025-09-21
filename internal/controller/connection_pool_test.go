package controller

import (
	"context"
	"testing"

	"memgraph-controller/internal/common"
)

func TestNewConnectionPool(t *testing.T) {
	config := &common.Config{}
	pool := NewConnectionPool(config)

	if pool == nil {
		t.Fatal("NewConnectionPool() returned nil")
	}

	if pool.config != config {
		t.Error("NewConnectionPool() did not store config correctly")
	}

	if pool.drivers == nil {
		t.Error("NewConnectionPool() did not initialize drivers map")
	}

	if len(pool.drivers) != 0 {
		t.Errorf("NewConnectionPool() drivers map length = %d, want 0", len(pool.drivers))
	}
}

func TestConnectionPool_GetDriver_EmptyAddress(t *testing.T) {
	config := &common.Config{}
	pool := NewConnectionPool(config)
	defer pool.Close(context.Background())

	_, err := pool.GetDriver(context.Background(), "")
	if err == nil {
		t.Error("Expected error for empty bolt address, got nil")
	}

	expectedMsg := "bolt address is empty"
	if err.Error() != expectedMsg {
		t.Errorf("Error message = %s, want %s", err.Error(), expectedMsg)
	}
}
func TestConnectionPool_Close(t *testing.T) {
	config := &common.Config{}
	pool := NewConnectionPool(config)

	// Close should not panic even with empty pool
	pool.Close(context.Background())

	// Verify pool is cleaned up
	if len(pool.drivers) != 0 {
		t.Errorf("After close, drivers map length = %d, want 0", len(pool.drivers))
	}
}

func TestConnectionPool_UpdatePodIP(t *testing.T) {
	config := &common.Config{}
	pool := NewConnectionPool(config)
	defer pool.Close(context.Background())

	// Test initial pod IP tracking
	pool.UpdatePodIP(context.Background(), "pod-1", "10.0.0.1")

	pool.mutex.RLock()
	ip, exists := pool.podIPs["pod-1"]
	pool.mutex.RUnlock()

	if !exists {
		t.Error("Pod IP was not stored")
	}
	if ip != "10.0.0.1" {
		t.Errorf("Pod IP = %s, want 10.0.0.1", ip)
	}
}

func TestConnectionPool_InvalidatePodConnection(t *testing.T) {
	config := &common.Config{}
	pool := NewConnectionPool(config)
	defer pool.Close(context.Background())

	// Track a pod IP
	pool.UpdatePodIP(context.Background(), "pod-1", "10.0.0.1")

	// Invalidate the connection
	pool.InvalidatePodConnection(context.Background(), "pod-1")

	// Verify the connection is cleaned up (no easy way to test without actually creating connections)
	// This test mainly ensures the method doesn't panic
}