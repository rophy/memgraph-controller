package controller

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"memgraph-controller/internal/common"
)

func TestMemgraphController_TestConnection(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	config := &common.Config{
		AppName:   "memgraph",
		Namespace: "memgraph",
	}

	ctrl := NewMemgraphController(fakeClientset, config)

	err := ctrl.TestConnection()
	if err != nil {
		t.Errorf("TestConnection() failed: %v", err)
	}
}