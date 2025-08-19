package main

import (
	"os"
	"testing"
	"time"

	"memgraph-controller/pkg/controller"

	"k8s.io/client-go/kubernetes/fake"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name     string
		envVars  map[string]string
		expected *controller.Config
	}{
		{
			name:    "default values",
			envVars: map[string]string{},
			expected: &controller.Config{
				AppName:            "memgraph",
				Namespace:          "memgraph",
				ReconcileInterval:  30 * time.Second,
				BoltPort:           "7687",
				ReplicationPort:    "10000",
				ServiceName:        "memgraph",
			},
		},
		{
			name: "custom values",
			envVars: map[string]string{
				"APP_NAME":            "custom-app",
				"NAMESPACE":           "custom-ns",
				"RECONCILE_INTERVAL":  "60s",
				"BOLT_PORT":           "8687",
				"REPLICATION_PORT":    "11000",
				"SERVICE_NAME":        "custom-service",
			},
			expected: &controller.Config{
				AppName:            "custom-app",
				Namespace:          "custom-ns",
				ReconcileInterval:  60 * time.Second,
				BoltPort:           "8687",
				ReplicationPort:    "11000",
				ServiceName:        "custom-service",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.envVars {
				os.Setenv(key, value)
			}
			defer func() {
				for key := range tt.envVars {
					os.Unsetenv(key)
				}
			}()

			config := controller.LoadConfig()

			if config.AppName != tt.expected.AppName {
				t.Errorf("AppName = %v, want %v", config.AppName, tt.expected.AppName)
			}
			if config.Namespace != tt.expected.Namespace {
				t.Errorf("Namespace = %v, want %v", config.Namespace, tt.expected.Namespace)
			}
			if config.ReconcileInterval != tt.expected.ReconcileInterval {
				t.Errorf("ReconcileInterval = %v, want %v", config.ReconcileInterval, tt.expected.ReconcileInterval)
			}
			if config.BoltPort != tt.expected.BoltPort {
				t.Errorf("BoltPort = %v, want %v", config.BoltPort, tt.expected.BoltPort)
			}
			if config.ReplicationPort != tt.expected.ReplicationPort {
				t.Errorf("ReplicationPort = %v, want %v", config.ReplicationPort, tt.expected.ReplicationPort)
			}
			if config.ServiceName != tt.expected.ServiceName {
				t.Errorf("ServiceName = %v, want %v", config.ServiceName, tt.expected.ServiceName)
			}
		})
	}
}

func TestMemgraphController_testConnection(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	config := &controller.Config{
		AppName:   "memgraph",
		Namespace: "memgraph",
	}

	ctrl := controller.NewMemgraphController(fakeClientset, config)

	err := ctrl.TestConnection()
	if err != nil {
		t.Errorf("TestConnection() failed: %v", err)
	}
}