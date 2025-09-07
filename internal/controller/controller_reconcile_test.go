package controller

import (
	"testing"
	"time"

	"memgraph-controller/internal/common"
)

// Tests for controller_reconcile.go (moved from reconciliation_test.go)

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