package controller

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestMemgraphController_TestConnection(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	config := &Config{
		AppName:   "memgraph",
		Namespace: "memgraph",
	}

	ctrl := NewMemgraphController(fakeClientset, config)

	err := ctrl.TestConnection()
	if err != nil {
		t.Errorf("TestConnection() failed: %v", err)
	}
}