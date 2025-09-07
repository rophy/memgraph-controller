package controller

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"memgraph-controller/internal/common"
)

// Tests for controller_events.go (moved from reconciliation_test.go)

func TestShouldReconcile(t *testing.T) {
	controller := &MemgraphController{
		config: &common.Config{StatefulSetName: "test-memgraph"},
	}

	tests := []struct {
		name            string
		oldPod          *v1.Pod
		newPod          *v1.Pod
		shouldReconcile bool
		reason          string
	}{
		{
			name: "phase change should reconcile",
			oldPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "memgraph-0"},
				Status:     v1.PodStatus{Phase: v1.PodPending},
			},
			newPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "memgraph-0"},
				Status:     v1.PodStatus{Phase: v1.PodRunning},
			},
			shouldReconcile: true,
			reason:          "phase change",
		},
		{
			name: "ip change should reconcile",
			oldPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "memgraph-0"},
				Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			newPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "memgraph-0"},
				Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.2"},
			},
			shouldReconcile: true,
			reason:          "IP change",
		},
		{
			name: "deletion timestamp should reconcile",
			oldPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "memgraph-0",
					DeletionTimestamp: nil,
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			newPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "memgraph-0",
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			shouldReconcile: true,
			reason:          "deletion timestamp",
		},
		{
			name: "node migration should reconcile",
			oldPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "memgraph-0"},
				Spec:       v1.PodSpec{NodeName: "node1"},
				Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			newPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "memgraph-0"},
				Spec:       v1.PodSpec{NodeName: "node2"},
				Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			shouldReconcile: true,
			reason:          "node migration",
		},
		{
			name: "resource version only should not reconcile",
			oldPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "memgraph-0",
					ResourceVersion: "1000",
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			newPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "memgraph-0",
					ResourceVersion: "1001",
				},
				Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			shouldReconcile: false,
			reason:          "only resource version changed",
		},
		{
			name: "no changes should not reconcile",
			oldPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "memgraph-0"},
				Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			newPod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "memgraph-0"},
				Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
			},
			shouldReconcile: false,
			reason:          "no significant changes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := controller.shouldReconcile(tt.oldPod, tt.newPod)
			if result != tt.shouldReconcile {
				t.Errorf("shouldReconcile() = %v, want %v for %s", result, tt.shouldReconcile, tt.reason)
			}
		})
	}
}