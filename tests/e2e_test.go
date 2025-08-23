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
	CurrentMaster     string            `json:"currentMaster"`
	TargetMasterIndex int               `json:"targetMasterIndex"`
	Pods              map[string]PodInfo `json:"pods"`
	Gateway           GatewayInfo       `json:"gateway,omitempty"`
}

type PodInfo struct {
	Name         string `json:"name"`
	BoltAddress  string `json:"boltAddress"`
	MemgraphRole string `json:"memgraphRole"`
	Healthy      bool   `json:"healthy"`
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
	expectedMaster   = "memgraph-0"
	expectedSyncReplica = "memgraph-1"
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
	assert.Equal(t, expectedMaster, status.CurrentMaster, "Master should be memgraph-0")
	assert.Equal(t, 0, status.TargetMasterIndex, "Target master index should be 0")
	
	// Validate gateway configuration
	if status.Gateway.Enabled {
		assert.Equal(t, status.CurrentMaster, status.Gateway.CurrentMaster, "Gateway should point to current master")
	}
	
	// Validate master-sync-async topology
	var masterCount, syncCount, asyncCount int
	var masterPod, syncPod string
	
	for podName, pod := range status.Pods {
		assert.NotEmpty(t, pod.BoltAddress, "Pod %s should have bolt address", podName)
		assert.True(t, pod.Healthy, "Pod %s should be healthy", podName)
		
		switch pod.MemgraphRole {
		case "main":
			masterCount++
			masterPod = podName
		case "replica":
			// Determine if this is sync or async replica by checking if it's memgraph-1
			if podName == expectedSyncReplica {
				syncCount++
				syncPod = podName
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
	
	// Store test data for replication test
	t.Setenv("E2E_TEST_ID", testID)
	t.Setenv("E2E_TEST_VALUE", testValue)
}

func TestE2E_DataReplicationVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	
	// Get test data from previous test
	testID := t.Getenv("E2E_TEST_ID")
	testValue := t.Getenv("E2E_TEST_VALUE")
	if testID == "" || testValue == "" {
		t.Skip("Skipping replication test - no test data from write test")
	}
	
	// Get cluster status to find all pod endpoints
	status, err := getClusterStatus(ctx)
	require.NoError(t, err, "Should get cluster status")
	
	// Wait for replication to complete (give it some time)
	time.Sleep(5 * time.Second)
	
	// Verify data exists on all pods
	var replicatedCount int
	for podName, pod := range status.Pods {
		if !pod.Healthy || pod.BoltAddress == "" {
			t.Logf("⚠ Skipping unhealthy pod %s", podName)
			continue
		}
		
		// Connect directly to each pod
		podURL := fmt.Sprintf("bolt://%s", pod.BoltAddress)
		driver, err := neo4j.NewDriverWithContext(podURL, neo4j.NoAuth())
		if err != nil {
			t.Logf("⚠ Failed to connect to pod %s: %v", podName, err)
			continue
		}
		defer driver.Close(ctx)
		
		// Try to read the test data from this pod
		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		result, err := session.Run(ctx,
			"MATCH (n:TestNode {id: $id}) RETURN n.id, n.value",
			map[string]interface{}{"id": testID})
		session.Close(ctx)
		
		if err != nil {
			t.Logf("⚠ Failed to query pod %s: %v", podName, err)
			continue
		}
		
		records, err := result.Collect(ctx)
		if err != nil {
			t.Logf("⚠ Failed to collect results from pod %s: %v", podName, err)
			continue
		}
		
		if len(records) == 1 {
			record := records[0]
			if record.Values[0] == testID && record.Values[1] == testValue {
				replicatedCount++
				t.Logf("✓ Data found on pod %s (%s)", podName, pod.MemgraphRole)
			} else {
				t.Logf("⚠ Data mismatch on pod %s", podName)
			}
		} else {
			t.Logf("⚠ Data not found on pod %s (%s)", podName, pod.MemgraphRole)
		}
	}
	
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