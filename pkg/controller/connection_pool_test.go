package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNewConnectionPool(t *testing.T) {
	config := &Config{}
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
	config := &Config{}
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
	config := &Config{}
	pool := NewConnectionPool(config)

	// Close should not panic even with empty pool
	pool.Close(context.Background())

	// Verify pool is cleaned up
	if len(pool.drivers) != 0 {
		t.Errorf("After close, drivers map length = %d, want 0", len(pool.drivers))
	}
}

func TestWithRetry_Success(t *testing.T) {
	retryConfig := RetryConfig{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
	}

	callCount := 0
	operation := func() error {
		callCount++
		return nil // Success on first try
	}

	ctx := context.Background()
	err := WithRetry(ctx, operation, retryConfig)

	if err != nil {
		t.Errorf("WithRetry() failed: %v", err)
	}

	if callCount != 1 {
		t.Errorf("Operation called %d times, want 1", callCount)
	}
}

func TestWithRetry_FailThenSuccess(t *testing.T) {
	retryConfig := RetryConfig{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
	}

	callCount := 0
	operation := func() error {
		callCount++
		if callCount < 3 {
			return fmt.Errorf("attempt %d failed", callCount)
		}
		return nil // Success on third try
	}

	ctx := context.Background()
	start := time.Now()
	err := WithRetry(ctx, operation, retryConfig)
	duration := time.Since(start)

	if err != nil {
		t.Errorf("WithRetry() failed: %v", err)
	}

	if callCount != 3 {
		t.Errorf("Operation called %d times, want 3", callCount)
	}

	// Should have some delay due to retries (at least 2 delays for 3 attempts)
	expectedMinDelay := 2 * retryConfig.BaseDelay
	if duration < expectedMinDelay {
		t.Errorf("Duration %v is less than expected minimum %v", duration, expectedMinDelay)
	}
}

func TestWithRetry_AllFailures(t *testing.T) {
	retryConfig := RetryConfig{
		MaxRetries: 2,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   100 * time.Millisecond,
	}

	callCount := 0
	testError := fmt.Errorf("persistent error")
	operation := func() error {
		callCount++
		return testError
	}

	ctx := context.Background()
	err := WithRetry(ctx, operation, retryConfig)

	if err == nil {
		t.Error("Expected WithRetry() to fail, got nil")
	}

	if callCount != 3 { // MaxRetries + 1
		t.Errorf("Operation called %d times, want 3", callCount)
	}

	// Error should mention the number of attempts
	errMsg := err.Error()
	if !strings.Contains(errMsg, "3 attempts") {
		t.Errorf("Error should mention 3 attempts, got: %s", errMsg)
	}
}

func TestWithRetry_ContextCancellation(t *testing.T) {
	retryConfig := RetryConfig{
		MaxRetries: 5,
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   1 * time.Second,
	}

	callCount := 0
	operation := func() error {
		callCount++
		return fmt.Errorf("always fails")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := WithRetry(ctx, operation, retryConfig)

	if err == nil {
		t.Error("Expected WithRetry() to fail due to context cancellation")
	}

	if err != context.DeadlineExceeded {
		t.Errorf("Expected context.DeadlineExceeded, got %v", err)
	}

	// Should have made at least one call
	if callCount == 0 {
		t.Error("Operation should have been called at least once")
	}
}

func TestWithRetry_ExponentialBackoff(t *testing.T) {
	retryConfig := RetryConfig{
		MaxRetries: 3,
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   1 * time.Second,
	}

	delays := []time.Duration{}
	callCount := 0
	lastCall := time.Now()

	operation := func() error {
		now := time.Now()
		if callCount > 0 {
			delays = append(delays, now.Sub(lastCall))
		}
		lastCall = now
		callCount++
		return fmt.Errorf("always fails")
	}

	ctx := context.Background()
	WithRetry(ctx, operation, retryConfig)

	if len(delays) != 3 { // 3 delays for 4 attempts
		t.Errorf("Expected 3 delays, got %d", len(delays))
		return
	}

	// Check that delays are increasing (exponential backoff)
	// First delay should be around BaseDelay
	expectedDelay1 := retryConfig.BaseDelay
	if delays[0] < expectedDelay1/2 || delays[0] > expectedDelay1*2 {
		t.Errorf("First delay %v should be around %v", delays[0], expectedDelay1)
	}

	// Second delay should be around 2 * BaseDelay
	expectedDelay2 := 2 * retryConfig.BaseDelay
	if delays[1] < expectedDelay2/2 || delays[1] > expectedDelay2*2 {
		t.Errorf("Second delay %v should be around %v", delays[1], expectedDelay2)
	}

	// Each subsequent delay should be longer (or equal due to max delay cap)
	for i := 1; i < len(delays); i++ {
		if delays[i] < delays[i-1]/2 { // Allow some variance
			t.Errorf("Delay %d (%v) should not be significantly less than delay %d (%v)", 
				i, delays[i], i-1, delays[i-1])
		}
	}
}