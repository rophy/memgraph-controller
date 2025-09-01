package controller

import (
	"testing"
)

func TestIsDataInfoReady(t *testing.T) {
	tests := []struct {
		name     string
		dataInfo string
		expected bool
	}{
		{
			name:     "ready_status_double_quotes",
			dataInfo: `{"memgraph":{"behind":0,"status":"ready","ts":123}}`,
			expected: true,
		},
		{
			name:     "ready_status_single_quotes", 
			dataInfo: `{"memgraph":{"behind":0,"status":'ready',"ts":123}}`,
			expected: true,
		},
		{
			name:     "empty_string",
			dataInfo: "",
			expected: false,
		},
		{
			name:     "empty_object",
			dataInfo: "{}",
			expected: false,
		},
		{
			name:     "null_string",
			dataInfo: "null",
			expected: false,
		},
		{
			name:     "null_capitalized",
			dataInfo: "Null",
			expected: false,
		},
		{
			name:     "error_condition",
			dataInfo: `{"memgraph":{"behind":0,"status":"error","ts":123}}`,
			expected: false,
		},
		{
			name:     "failed_condition",
			dataInfo: `{"memgraph":{"behind":0,"status":"failed","ts":123}}`,
			expected: false,
		},
		{
			name:     "disconnected_condition",
			dataInfo: `{"memgraph":{"behind":0,"status":"disconnected","ts":123}}`,
			expected: false,
		},
		{
			name:     "timeout_condition",
			dataInfo: `{"memgraph":{"behind":0,"status":"timeout","ts":123}}`,
			expected: false,
		},
		{
			name:     "ready_with_error_should_be_false",
			dataInfo: `{"memgraph":{"behind":0,"status":"ready","error":"connection failed","ts":123}}`,
			expected: false,
		},
		{
			name:     "no_status_field",
			dataInfo: `{"memgraph":{"behind":0,"ts":123}}`,
			expected: false,
		},
	}

	r := &ReconcileActions{}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := r.isDataInfoReady(tt.dataInfo)
			if result != tt.expected {
				t.Errorf("isDataInfoReady(%q) = %v, expected %v", tt.dataInfo, result, tt.expected)
			}
		})
	}
}

func TestStep6CheckAsyncReplicasDataInfo_Logic(t *testing.T) {
	// Test the logic of ASYNC replica health checking without mocking complex dependencies
	tests := []struct {
		name          string
		replicatList  map[string]ReplicaInfo
		expectedAsyncHealthy int
		expectedAsyncUnhealthy int
	}{
		{
			name: "all_async_replicas_healthy",
			replicatList: map[string]ReplicaInfo{
				"memgraph_ha_2": {
					SyncMode: "async",
					DataInfo: `{"memgraph":{"behind":0,"status":"ready","ts":123}}`,
				},
				"memgraph_ha_3": {
					SyncMode: "async", 
					DataInfo: `{"memgraph":{"behind":0,"status":"ready","ts":456}}`,
				},
			},
			expectedAsyncHealthy: 2,
			expectedAsyncUnhealthy: 0,
		},
		{
			name: "mixed_healthy_unhealthy_async",
			replicatList: map[string]ReplicaInfo{
				"memgraph_ha_2": {
					SyncMode: "async",
					DataInfo: `{"memgraph":{"behind":0,"status":"ready","ts":123}}`,
				},
				"memgraph_ha_3": {
					SyncMode: "async",
					DataInfo: `{"memgraph":{"behind":0,"status":"error","ts":456}}`,
				},
			},
			expectedAsyncHealthy: 1,
			expectedAsyncUnhealthy: 1,
		},
		{
			name: "all_async_replicas_unhealthy",
			replicatList: map[string]ReplicaInfo{
				"memgraph_ha_2": {
					SyncMode: "async",
					DataInfo: "",
				},
				"memgraph_ha_3": {
					SyncMode: "async",
					DataInfo: "{}",
				},
			},
			expectedAsyncHealthy: 0,
			expectedAsyncUnhealthy: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReconcileActions{}
			
			// Count healthy vs unhealthy ASYNC replicas using the same logic as step6
			asyncHealthy := 0
			asyncUnhealthy := 0
			
			for _, replica := range tt.replicatList {
				if replica.SyncMode == "async" {
					if r.isDataInfoReady(replica.DataInfo) {
						asyncHealthy++
					} else {
						asyncUnhealthy++
					}
				}
			}
			
			if asyncHealthy != tt.expectedAsyncHealthy {
				t.Errorf("Expected %d healthy ASYNC replicas, got %d", tt.expectedAsyncHealthy, asyncHealthy)
			}
			if asyncUnhealthy != tt.expectedAsyncUnhealthy {
				t.Errorf("Expected %d unhealthy ASYNC replicas, got %d", tt.expectedAsyncUnhealthy, asyncUnhealthy)
			}
		})
	}
}

func TestStep8ValidateFinalResult_Logic(t *testing.T) {
	// Test the health validation logic used in Step 8
	tests := []struct {
		name                      string
		replicatList             map[string]ReplicaInfo
		expectedSyncHealthy      int
		expectedAsyncHealthy     int 
		expectedAsyncUnhealthy   int
	}{
		{
			name: "healthy_sync_and_async_replicas",
			replicatList: map[string]ReplicaInfo{
				"memgraph_ha_1": {
					SyncMode: "sync",
					DataInfo: `{"memgraph":{"behind":0,"status":"ready","ts":123}}`,
				},
				"memgraph_ha_2": {
					SyncMode: "async",
					DataInfo: `{"memgraph":{"behind":0,"status":"ready","ts":456}}`,
				},
			},
			expectedSyncHealthy:    1,
			expectedAsyncHealthy:   1,
			expectedAsyncUnhealthy: 0,
		},
		{
			name: "unhealthy_replicas",
			replicatList: map[string]ReplicaInfo{
				"memgraph_ha_1": {
					SyncMode: "sync",
					DataInfo: "",
				},
				"memgraph_ha_2": {
					SyncMode: "async",
					DataInfo: "{}",
				},
				"memgraph_ha_3": {
					SyncMode: "async",
					DataInfo: `{"memgraph":{"behind":0,"status":"error","ts":789}}`,
				},
			},
			expectedSyncHealthy:    0,
			expectedAsyncHealthy:   0,
			expectedAsyncUnhealthy: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &ReconcileActions{}
			
			// Test the same validation logic as step8
			syncHealthy := 0
			asyncHealthy := 0
			asyncUnhealthy := 0
			
			for _, replica := range tt.replicatList {
				if replica.SyncMode == "sync" {
					if r.isDataInfoReady(replica.DataInfo) {
						syncHealthy++
					}
				} else if replica.SyncMode == "async" {
					if r.isDataInfoReady(replica.DataInfo) {
						asyncHealthy++
					} else {
						asyncUnhealthy++
					}
				}
			}
			
			if syncHealthy != tt.expectedSyncHealthy {
				t.Errorf("Expected %d healthy SYNC replicas, got %d", tt.expectedSyncHealthy, syncHealthy)
			}
			if asyncHealthy != tt.expectedAsyncHealthy {
				t.Errorf("Expected %d healthy ASYNC replicas, got %d", tt.expectedAsyncHealthy, asyncHealthy)
			}
			if asyncUnhealthy != tt.expectedAsyncUnhealthy {
				t.Errorf("Expected %d unhealthy ASYNC replicas, got %d", tt.expectedAsyncUnhealthy, asyncUnhealthy)
			}
		})
	}
}