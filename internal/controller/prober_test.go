package controller

import (
	"context"
	"testing"
	"time"
)

// Test the ProberConfig defaults
func TestDefaultProberConfig(t *testing.T) {
	config := DefaultProberConfig()
	
	if config.CheckInterval != 5*time.Second {
		t.Errorf("Expected CheckInterval 5s, got %v", config.CheckInterval)
	}
	if config.Timeout != 3*time.Second {
		t.Errorf("Expected Timeout 3s, got %v", config.Timeout)
	}
	if config.FailureThreshold != 3 {
		t.Errorf("Expected FailureThreshold 3, got %v", config.FailureThreshold)
	}
}

// Test NewHealthProber constructor
func TestNewHealthProber(t *testing.T) {
	// Create a minimal mock controller (we'll use nil since constructor doesn't use methods)
	var mockController *MemgraphController = nil
	
	config := ProberConfig{
		CheckInterval:    1 * time.Second,
		Timeout:          500 * time.Millisecond,
		FailureThreshold: 2,
	}
	
	prober := NewHealthProber(mockController, config)
	
	if prober.controller != mockController {
		t.Error("Controller not set correctly")
	}
	if prober.config.CheckInterval != config.CheckInterval {
		t.Error("Config CheckInterval not set correctly")
	}
	if prober.config.Timeout != config.Timeout {
		t.Error("Config Timeout not set correctly")
	}
	if prober.config.FailureThreshold != config.FailureThreshold {
		t.Error("Config FailureThreshold not set correctly")
	}
	if prober.running {
		t.Error("Prober should not be running initially")
	}
	if prober.consecutiveFailures != 0 {
		t.Error("Consecutive failures should be 0 initially")
	}
	if !prober.lastHealthStatus {
		t.Error("Last health status should be true initially")
	}
}

// Test basic start/stop functionality
func TestHealthProber_StartStop(t *testing.T) {
	var mockController *MemgraphController = nil
	config := ProberConfig{
		CheckInterval:    100 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 2,
	}
	
	prober := NewHealthProber(mockController, config)
	
	// Test initial state
	if prober.IsRunning() {
		t.Error("Prober should not be running initially")
	}
	
	// Start prober (will fail health checks due to nil controller, but that's ok for this test)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	prober.Start(ctx)
	
	if !prober.IsRunning() {
		t.Error("Prober should be running after Start()")
	}
	
	// Wait a bit to ensure it's actually running
	time.Sleep(20 * time.Millisecond)
	
	// Stop prober
	prober.Stop()
	
	if prober.IsRunning() {
		t.Error("Prober should not be running after Stop()")
	}
}

// Test starting prober twice
func TestHealthProber_StartTwice(t *testing.T) {
	var mockController *MemgraphController = nil
	config := ProberConfig{
		CheckInterval:    100 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 2,
	}
	
	prober := NewHealthProber(mockController, config)
	ctx := context.Background()
	
	// Start first time
	prober.Start(ctx)
	if !prober.IsRunning() {
		t.Error("Prober should be running after first Start()")
	}
	
	// Start second time - should not panic or cause issues
	prober.Start(ctx)
	if !prober.IsRunning() {
		t.Error("Prober should still be running after second Start()")
	}
	
	prober.Stop()
}

// Test health status tracking
func TestHealthProber_GetHealthStatus(t *testing.T) {
	var mockController *MemgraphController = nil
	config := ProberConfig{
		CheckInterval:    100 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 2,
	}
	
	prober := NewHealthProber(mockController, config)
	
	// Test initial status
	healthy, failures := prober.GetHealthStatus()
	if !healthy {
		t.Error("Should be healthy initially")
	}
	if failures != 0 {
		t.Error("Should have 0 failures initially")
	}
	
	// Simulate failure
	prober.recordFailure()
	healthy, failures = prober.GetHealthStatus()
	if healthy {
		t.Error("Should not be healthy after failure")
	}
	if failures != 1 {
		t.Errorf("Should have 1 failure, got %d", failures)
	}
	
	// Simulate success
	prober.recordSuccess("test-pod")
	healthy, failures = prober.GetHealthStatus()
	if !healthy {
		t.Error("Should be healthy after success")
	}
	if failures != 0 {
		t.Errorf("Should have 0 failures after success, got %d", failures)
	}
}

// Test failure recovery reporting
func TestHealthProber_RecoveryLogging(t *testing.T) {
	var mockController *MemgraphController = nil
	config := ProberConfig{
		CheckInterval:    100 * time.Millisecond,
		Timeout:          50 * time.Millisecond,
		FailureThreshold: 3,
	}
	
	prober := NewHealthProber(mockController, config)
	
	// Record some failures
	prober.recordFailure()
	prober.recordFailure()
	
	healthy, failures := prober.GetHealthStatus()
	if healthy || failures != 2 {
		t.Error("Should have 2 failures and be unhealthy")
	}
	
	// Recovery should reset failures to 0
	prober.recordSuccess("test-pod")
	healthy, failures = prober.GetHealthStatus()
	if !healthy || failures != 0 {
		t.Error("Should be healthy with 0 failures after recovery")
	}
}

// Test that failure threshold resets consecutiveFailures correctly
func TestHealthProber_FailureThresholdReset(t *testing.T) {
	var mockController *MemgraphController = nil
	
	config := ProberConfig{
		CheckInterval:    10 * time.Millisecond,
		Timeout:          5 * time.Millisecond,
		FailureThreshold: 3,
	}
	
	prober := NewHealthProber(mockController, config)
	
	// Test that consecutive failures accumulate
	prober.recordFailure() // 1st failure
	healthy, failures := prober.GetHealthStatus()
	if healthy || failures != 1 {
		t.Error("Should have 1 failure and be unhealthy")
	}
	
	prober.recordFailure() // 2nd failure
	healthy, failures = prober.GetHealthStatus()
	if healthy || failures != 2 {
		t.Error("Should have 2 failures and be unhealthy")
	}
	
	// 3rd failure should trigger failover and reset counter
	// Note: We can't easily test the actual failover trigger without complex mocking,
	// but we can test that the failure logic works correctly
	prober.recordFailure() // 3rd failure - should reset
	
	// Give some time for potential goroutine execution
	time.Sleep(20 * time.Millisecond)
	
	// The consecutive failures should have been reset to 0 after threshold
	healthy, failures = prober.GetHealthStatus()
	if healthy || failures != 0 {
		t.Errorf("Should have 0 failures after threshold reset, got %d", failures)
	}
}

// Test context cancellation
func TestHealthProber_ContextCancellation(t *testing.T) {
	var mockController *MemgraphController = nil
	config := ProberConfig{
		CheckInterval:    10 * time.Millisecond,
		Timeout:          5 * time.Millisecond,
		FailureThreshold: 3,
	}
	
	prober := NewHealthProber(mockController, config)
	
	ctx, cancel := context.WithCancel(context.Background())
	
	// Start prober
	prober.Start(ctx)
	if !prober.IsRunning() {
		t.Error("Prober should be running")
	}
	
	// Cancel context
	cancel()
	
	// Give some time for the goroutine to stop
	time.Sleep(50 * time.Millisecond)
	
	// Prober should still show as running (only Stop() changes this flag)
	// But the goroutine should have exited due to context cancellation
	if !prober.IsRunning() {
		t.Error("Prober running flag should still be true after context cancellation")
	}
	
	// Clean up
	prober.Stop()
}

// Test timeout functionality
func TestHealthProber_TimeoutConfig(t *testing.T) {
	config := ProberConfig{
		CheckInterval:    1 * time.Second,
		Timeout:          100 * time.Millisecond, // Short timeout
		FailureThreshold: 1,
	}
	
	// Test that config values are preserved
	if config.Timeout != 100*time.Millisecond {
		t.Errorf("Expected timeout 100ms, got %v", config.Timeout)
	}
	
	var mockController *MemgraphController = nil
	prober := NewHealthProber(mockController, config)
	
	if prober.config.Timeout != 100*time.Millisecond {
		t.Errorf("Prober should preserve timeout config, got %v", prober.config.Timeout)
	}
}

