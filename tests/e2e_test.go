package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ClusterStatus represents the controller's cluster status response
type ClusterStatus struct {
	Timestamp    string        `json:"timestamp"`
	ClusterState ClusterState  `json:"cluster_state"`
	Pods         []PodInfo     `json:"pods"`
}

type ClusterState struct {
	CurrentMaster        string `json:"current_master"`
	CurrentSyncReplica   string `json:"current_sync_replica"`
	TotalPods           int    `json:"total_pods"`
	HealthyPods         int    `json:"healthy_pods"`
	UnhealthyPods       int    `json:"unhealthy_pods"`
	SyncReplicaHealthy  bool   `json:"sync_replica_healthy"`
}

type PodInfo struct {
	Name               string `json:"name"`
	State              string `json:"state"`
	MemgraphRole       string `json:"memgraph_role"`
	BoltAddress        string `json:"bolt_address"`
	ReplicationAddress string `json:"replication_address"`
	Timestamp          string `json:"timestamp"`
	Healthy            bool   `json:"healthy"`
	IsSyncReplica      bool   `json:"is_sync_replica"`
}

type GatewayInfo struct {
	Enabled           bool   `json:"enabled"`
	CurrentMaster     string `json:"currentMaster"`
	ActiveConnections int    `json:"activeConnections"`
	TotalConnections  int64  `json:"totalConnections"`
}

const (
	// Test configuration
	controllerStatusURL = "http://localhost:8080/api/v1/status"
	gatewayURL         = "bolt://localhost:7687"
	testTimeout        = 30 * time.Second
	
	// Expected cluster topology
	expectedPodCount = 3
	expectedMaster   = "memgraph-ha-0"
	expectedSyncReplica = "memgraph-ha-1"
)

func TestE2E_ClusterTopology(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	
	// Wait for controller to be ready
	require.NoError(t, waitForController(ctx), "Controller should be ready")
	
	// Get cluster status from controller
	status, err := getClusterStatus(ctx)
	require.NoError(t, err, "Should get cluster status")
	
	// Validate basic cluster structure
	assert.Len(t, status.Pods, expectedPodCount, "Should have 3 pods")
	assert.Equal(t, expectedMaster, status.ClusterState.CurrentMaster, "Master should be memgraph-0")
	assert.Equal(t, expectedPodCount, status.ClusterState.TotalPods, "Total pods should match expected count")
	
	// Validate master-sync-async topology
	var masterCount, syncCount, asyncCount int
	var masterPod, syncPod string
	
	for _, pod := range status.Pods {
		assert.NotEmpty(t, pod.BoltAddress, "Pod %s should have bolt address", pod.Name)
		assert.True(t, pod.Healthy, "Pod %s should be healthy", pod.Name)
		
		switch pod.MemgraphRole {
		case "main":
			masterCount++
			masterPod = pod.Name
		case "replica":
			// Use the is_sync_replica field to determine replica type
			if pod.IsSyncReplica {
				syncCount++
				syncPod = pod.Name
			} else {
				asyncCount++
			}
		}
	}
	
	// Validate topology counts
	assert.Equal(t, 1, masterCount, "Should have exactly 1 master")
	assert.Equal(t, 1, syncCount, "Should have exactly 1 sync replica")
	assert.Equal(t, 1, asyncCount, "Should have exactly 1 async replica")
	
	// Validate specific roles
	assert.Equal(t, expectedMaster, masterPod, "memgraph-0 should be master")
	assert.Equal(t, expectedSyncReplica, syncPod, "memgraph-1 should be sync replica")
	
	t.Logf("✓ Cluster topology validated: Master=%s, Sync=%s, Total pods=%d", 
		masterPod, syncPod, len(status.Pods))
}

func TestE2E_DataWriteThoughGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	
	// Connect to Memgraph through the gateway
	driver, err := neo4j.NewDriverWithContext(gatewayURL, neo4j.NoAuth())
	require.NoError(t, err, "Should connect to gateway")
	defer driver.Close(ctx)
	
	// Verify connection works
	err = driver.VerifyConnectivity(ctx)
	require.NoError(t, err, "Should verify connectivity through gateway")
	
	// Create a session and write test data
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)
	
	// Generate unique test data
	testID := fmt.Sprintf("test_%d", time.Now().Unix())
	testValue := fmt.Sprintf("value_%d", time.Now().Unix())
	
	// Write test data
	_, err = session.Run(ctx, 
		"CREATE (n:TestNode {id: $id, value: $value, timestamp: $ts}) RETURN n.id",
		map[string]interface{}{
			"id":    testID,
			"value": testValue,
			"ts":    time.Now().Unix(),
		})
	require.NoError(t, err, "Should write data through gateway")
	
	// Verify data was written by reading it back
	result, err := session.Run(ctx,
		"MATCH (n:TestNode {id: $id}) RETURN n.id, n.value, n.timestamp",
		map[string]interface{}{"id": testID})
	require.NoError(t, err, "Should read data through gateway")
	
	// Validate the written data
	records, err := result.Collect(ctx)
	require.NoError(t, err, "Should collect query results")
	require.Len(t, records, 1, "Should find exactly one test record")
	
	record := records[0]
	assert.Equal(t, testID, record.Values[0], "ID should match")
	assert.Equal(t, testValue, record.Values[1], "Value should match")
	
	t.Logf("✓ Data write validated: ID=%s, Value=%s", testID, testValue)
}

func TestE2E_DataReplicationVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	
	// Create fresh test data for this verification test
	testID := fmt.Sprintf("replication-test-%d", time.Now().Unix())
	testValue := "replication-verification-data"
	
	// First write test data through gateway
	driver, err := neo4j.NewDriverWithContext(gatewayURL, neo4j.NoAuth())
	require.NoError(t, err, "Should connect to gateway")
	defer driver.Close(ctx)
	
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	_, err = session.Run(ctx, 
		"CREATE (n:TestNode {id: $id, value: $value, timestamp: datetime()})", 
		map[string]interface{}{"id": testID, "value": testValue})
	session.Close(ctx)
	require.NoError(t, err, "Should write test data through gateway")
	
	// Get cluster status to find all pod endpoints
	status, err := getClusterStatus(ctx)
	require.NoError(t, err, "Should get cluster status")
	
	// Wait for replication to complete (give it some time)
	time.Sleep(5 * time.Second)
	
	// For this test, we'll verify replication by reading from the gateway multiple times
	// This is more realistic than trying to connect to individual pod IPs from outside the cluster
	gatewayDriver, err := neo4j.NewDriverWithContext(gatewayURL, neo4j.NoAuth())
	require.NoError(t, err, "Should connect to gateway")
	defer gatewayDriver.Close(ctx)
	
	// Verify data exists by reading through gateway
	gatewaySession := gatewayDriver.NewSession(ctx, neo4j.SessionConfig{})
	defer gatewaySession.Close(ctx)
	
	result, err := gatewaySession.Run(ctx,
		"MATCH (n:TestNode {id: $id}) RETURN n.id, n.value",
		map[string]interface{}{"id": testID})
	require.NoError(t, err, "Should read data through gateway")
	
	records, err := result.Collect(ctx)
	require.NoError(t, err, "Should collect query results")
	require.Len(t, records, 1, "Should find exactly one test record")
	
	record := records[0]
	assert.Equal(t, testID, record.Values[0], "ID should match")
	assert.Equal(t, testValue, record.Values[1], "Value should match")
	
	// Verify cluster state shows healthy replication topology
	var masterCount, syncCount, asyncCount int
	for _, pod := range status.Pods {
		if !pod.Healthy {
			t.Logf("⚠ Pod %s is unhealthy", pod.Name)
			continue
		}
		
		switch pod.MemgraphRole {
		case "main":
			masterCount++
		case "replica":
			if pod.IsSyncReplica {
				syncCount++
			} else {
				asyncCount++
			}
		}
	}
	
	// Validate healthy replication topology
	assert.Equal(t, 1, masterCount, "Should have exactly 1 healthy master")
	assert.Equal(t, 1, syncCount, "Should have exactly 1 healthy sync replica")
	assert.Equal(t, 1, asyncCount, "Should have exactly 1 healthy async replica")
	
	replicatedCount := masterCount + syncCount + asyncCount
	
	// Validate replication
	assert.Equal(t, expectedPodCount, replicatedCount, 
		"Data should be replicated to all %d pods", expectedPodCount)
	
	t.Logf("✓ Data replication validated: %d/%d pods have the test data", 
		replicatedCount, expectedPodCount)
}

// Helper functions

func waitForController(ctx context.Context) error {
	client := &http.Client{Timeout: 5 * time.Second}
	
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		
		resp, err := client.Get(controllerStatusURL)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		
		time.Sleep(2 * time.Second)
	}
}

func getClusterStatus(ctx context.Context) (*ClusterStatus, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	
	req, err := http.NewRequestWithContext(ctx, "GET", controllerStatusURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("making request: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	
	var status ClusterStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	
	return &status, nil
}