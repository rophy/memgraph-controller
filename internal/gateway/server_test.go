package gateway

import (
	"sync"
	"testing"
	"time"
)

func TestServer_SetUpstreamAddress(t *testing.T) {
	config := &Config{
		BindAddress:            "localhost:0", // Use port 0 for auto-assignment
		MaxConnections:         10,
		Timeout:                30 * time.Second,
		ConnectionTimeout:      5 * time.Second,
		BufferSize:             32768,
		IdleTimeout:            5 * time.Minute,
		CleanupInterval:        1 * time.Minute,
		MaxBytesPerConnection:  100 * 1024 * 1024,
		TLSEnabled:             false,
		RateLimitEnabled:       false,
		RateLimitRPS:           100,
		RateLimitBurst:         200,
		RateLimitWindow:        time.Minute,
		HealthCheckInterval:    30 * time.Second,
		TraceEnabled:           false,
	}

	server := NewServer(config)

	// Test initial empty state
	if addr := server.GetUpstreamAddress(); addr != "" {
		t.Errorf("Expected empty upstream address, got %s", addr)
	}

	// Test setting upstream address
	testAddress := "10.0.0.1:7687"
	server.SetUpstreamAddress(testAddress)
	
	if addr := server.GetUpstreamAddress(); addr != testAddress {
		t.Errorf("Expected upstream address %s, got %s", testAddress, addr)
	}

	// Test idempotent behavior - setting same address should not cause change
	server.SetUpstreamAddress(testAddress)
	if addr := server.GetUpstreamAddress(); addr != testAddress {
		t.Errorf("Expected upstream address to remain %s, got %s", testAddress, addr)
	}

	// Test changing to different address
	newAddress := "10.0.0.2:7687"
	server.SetUpstreamAddress(newAddress)
	if addr := server.GetUpstreamAddress(); addr != newAddress {
		t.Errorf("Expected upstream address %s, got %s", newAddress, addr)
	}

	// Test resetting to empty
	server.SetUpstreamAddress("")
	if addr := server.GetUpstreamAddress(); addr != "" {
		t.Errorf("Expected empty upstream address after reset, got %s", addr)
	}
}

func TestServer_ConcurrentUpstreamAccess(t *testing.T) {
	config := &Config{
		BindAddress:            "localhost:0",
		MaxConnections:         10,
		Timeout:                30 * time.Second,
		ConnectionTimeout:      5 * time.Second,
		BufferSize:             32768,
		IdleTimeout:            5 * time.Minute,
		CleanupInterval:        1 * time.Minute,
		MaxBytesPerConnection:  100 * 1024 * 1024,
		TLSEnabled:             false,
		RateLimitEnabled:       false,
		RateLimitRPS:           100,
		RateLimitBurst:         200,
		RateLimitWindow:        time.Minute,
		HealthCheckInterval:    30 * time.Second,
		TraceEnabled:           false,
	}

	server := NewServer(config)
	
	const numGoroutines = 10
	const numOperations = 100
	
	var wg sync.WaitGroup
	wg.Add(numGoroutines * 2) // readers + writers

	// Start readers
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				_ = server.GetUpstreamAddress()
			}
		}()
	}

	// Start writers
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				addr := ""
				if j%2 == 0 {
					addr = "10.0.0.1:7687"
				} else {
					addr = "10.0.0.2:7687"
				}
				server.SetUpstreamAddress(addr)
			}
		}(i)
	}

	wg.Wait()

	// Test should complete without race conditions or panics
	// Final state doesn't matter as much as ensuring no races
	finalAddr := server.GetUpstreamAddress()
	t.Logf("Final upstream address: %s", finalAddr)
}

func TestServer_GetUpstreamAddressThreadSafety(t *testing.T) {
	config := &Config{
		BindAddress:            "localhost:0",
		MaxConnections:         10,
		Timeout:                30 * time.Second,
		ConnectionTimeout:      5 * time.Second,
		BufferSize:             32768,
		IdleTimeout:            5 * time.Minute,
		CleanupInterval:        1 * time.Minute,
		MaxBytesPerConnection:  100 * 1024 * 1024,
		TLSEnabled:             false,
		RateLimitEnabled:       false,
		RateLimitRPS:           100,
		RateLimitBurst:         200,
		RateLimitWindow:        time.Minute,
		HealthCheckInterval:    30 * time.Second,
		TraceEnabled:           false,
	}

	server := NewServer(config)
	
	// Set an initial address
	testAddress := "10.0.0.1:7687"
	server.SetUpstreamAddress(testAddress)

	// Test concurrent reads
	const numReaders = 50
	var wg sync.WaitGroup
	wg.Add(numReaders)

	results := make([]string, numReaders)
	
	for i := 0; i < numReaders; i++ {
		go func(index int) {
			defer wg.Done()
			results[index] = server.GetUpstreamAddress()
		}(i)
	}

	wg.Wait()

	// All reads should return the same value
	for i, result := range results {
		if result != testAddress {
			t.Errorf("Reader %d got %s, expected %s", i, result, testAddress)
		}
	}
}

func TestNewServer(t *testing.T) {
	config := &Config{
		BindAddress:            "localhost:0",
		MaxConnections:         10,
		Timeout:                30 * time.Second,
		ConnectionTimeout:      5 * time.Second,
		BufferSize:             32768,
		IdleTimeout:            5 * time.Minute,
		CleanupInterval:        1 * time.Minute,
		MaxBytesPerConnection:  100 * 1024 * 1024,
		TLSEnabled:             false,
		RateLimitEnabled:       false,
		RateLimitRPS:           100,
		RateLimitBurst:         200,
		RateLimitWindow:        time.Minute,
		HealthCheckInterval:    30 * time.Second,
		TraceEnabled:           false,
	}

	server := NewServer(config)

	if server == nil {
		t.Fatal("NewServer returned nil")
	}

	if server.config != config {
		t.Error("Server config not set correctly")
	}

	if server.GetUpstreamAddress() != "" {
		t.Error("Server should start with empty upstream address")
	}

	if server.connections == nil {
		t.Error("Server connections tracker not initialized")
	}

	if server.rateLimiter == nil {
		t.Error("Server rate limiter not initialized")
	}
}