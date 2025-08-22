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
	controller := &MemgraphController{
		metrics: &ReconciliationMetrics{},
	}

	// Test successful reconciliation
	controller.updateReconciliationMetrics("test-success", time.Millisecond*100, nil)

	if controller.metrics.TotalReconciliations != 1 {
		t.Errorf("TotalReconciliations = %d, want 1", controller.metrics.TotalReconciliations)
	}
	if controller.metrics.SuccessfulReconciliations != 1 {
		t.Errorf("SuccessfulReconciliations = %d, want 1", controller.metrics.SuccessfulReconciliations)
	}
	if controller.metrics.FailedReconciliations != 0 {
		t.Errorf("FailedReconciliations = %d, want 0", controller.metrics.FailedReconciliations)
	}
	if controller.metrics.LastReconciliationReason != "test-success" {
		t.Errorf("LastReconciliationReason = %s, want test-success", controller.metrics.LastReconciliationReason)
	}
	if controller.metrics.LastReconciliationError != "" {
		t.Errorf("LastReconciliationError = %s, want empty", controller.metrics.LastReconciliationError)
	}

	// Test failed reconciliation
	testErr := "test error"
	controller.updateReconciliationMetrics("test-failure", time.Millisecond*50, 
		&testError{msg: testErr})

	if controller.metrics.TotalReconciliations != 2 {
		t.Errorf("TotalReconciliations = %d, want 2", controller.metrics.TotalReconciliations)
	}
	if controller.metrics.SuccessfulReconciliations != 1 {
		t.Errorf("SuccessfulReconciliations = %d, want 1", controller.metrics.SuccessfulReconciliations)
	}
	if controller.metrics.FailedReconciliations != 1 {
		t.Errorf("FailedReconciliations = %d, want 1", controller.metrics.FailedReconciliations)
	}
	if controller.metrics.LastReconciliationReason != "test-failure" {
		t.Errorf("LastReconciliationReason = %s, want test-failure", controller.metrics.LastReconciliationReason)
	}
	if controller.metrics.LastReconciliationError != testErr {
		t.Errorf("LastReconciliationError = %s, want %s", controller.metrics.LastReconciliationError, testErr)
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

func TestIsNonRetryableError(t *testing.T) {
	controller := &MemgraphController{}

	tests := []struct {
		name        string
		err         error
		nonRetryable bool
	}{
		{
			name:        "manual_intervention_required",
			err:         &testError{msg: "manual intervention required"},
			nonRetryable: true,
		},
		{
			name:        "ambiguous_cluster_state",
			err:         &testError{msg: "ambiguous cluster state detected"},
			nonRetryable: true,
		},
		{
			name:        "regular_error",
			err:         &testError{msg: "connection failed"},
			nonRetryable: false,
		},
		{
			name:        "timeout_error",
			err:         &testError{msg: "timeout exceeded"},
			nonRetryable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.isNonRetryableError(tt.err)
			if result != tt.nonRetryable {
				t.Errorf("isNonRetryableError() = %v, want %v", result, tt.nonRetryable)
			}
		})
	}
}

// testError is a simple error implementation for testing
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}