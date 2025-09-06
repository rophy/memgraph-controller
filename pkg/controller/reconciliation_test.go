package controller

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestShouldReconcile(t *testing.T) {
	controller := &MemgraphController{
		config: &Config{StatefulSetName: "test-memgraph"},
	}

	tests := []struct {
		name            string
		oldPod          *v1.Pod
		newPod          *v1.Pod
		shouldReconcile bool
	}{
		{
			name: "phase_change_should_reconcile",
			oldPod: &v1.Pod{
				Status: v1.PodStatus{Phase: v1.PodPending},
			},
			newPod: &v1.Pod{
				Status: v1.PodStatus{Phase: v1.PodRunning},
			},
			shouldReconcile: true,
		},
		{
			name: "ip_change_should_reconcile",
			oldPod: &v1.Pod{
				Status: v1.PodStatus{PodIP: "10.0.0.1"},
			},
			newPod: &v1.Pod{
				Status: v1.PodStatus{PodIP: "10.0.0.2"},
			},
			shouldReconcile: true,
		},
		{
			name: "deletion_timestamp_should_reconcile",
			oldPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: nil},
			},
			newPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
			},
			shouldReconcile: true,
		},
		{
			name: "node_migration_should_reconcile",
			oldPod: &v1.Pod{
				Spec: v1.PodSpec{NodeName: "node-1"},
			},
			newPod: &v1.Pod{
				Spec: v1.PodSpec{NodeName: "node-2"},
			},
			shouldReconcile: true,
		},
		{
			name: "resource_version_only_should_not_reconcile",
			oldPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"},
				Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
				Spec:       v1.PodSpec{NodeName: "node-1"},
			},
			newPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"},
				Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
				Spec:       v1.PodSpec{NodeName: "node-1"},
			},
			shouldReconcile: false,
		},
		{
			name: "no_changes_should_not_reconcile",
			oldPod: &v1.Pod{
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
				Spec:   v1.PodSpec{NodeName: "node-1"},
			},
			newPod: &v1.Pod{
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
				Spec:   v1.PodSpec{NodeName: "node-1"},
			},
			shouldReconcile: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.shouldReconcile(tt.oldPod, tt.newPod)
			if result != tt.shouldReconcile {
				t.Errorf("shouldReconcile() = %v, want %v", result, tt.shouldReconcile)
			}
		})
	}
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
}

func TestControllerBasics(t *testing.T) {
	// Test basic controller functionality that doesn't require complex setup
	controller := &MemgraphController{}

	// Test GetReconciliationMetrics with nil metrics
	metrics := controller.GetReconciliationMetrics()
	if metrics.TotalReconciliations != 0 {
		t.Errorf("GetReconciliationMetrics with nil metrics should return zero values")
	}
}

