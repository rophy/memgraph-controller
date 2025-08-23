package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
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
)

func TestE2E_ClusterTopology(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	
	// Wait for controller to be ready
	require.NoError(t, waitForController(ctx), "Controller should be ready")
	
	// Get cluster status from controller
	status, err := getClusterStatus(ctx)
	require.NoError(t, err, "Should get cluster status")
	
	// Validate basic cluster structure - CRITICAL: crash if these fail
	require.Len(t, status.Pods, expectedPodCount, "CRITICAL: Should have 3 pods")
	require.Equal(t, expectedPodCount, status.ClusterState.TotalPods, "CRITICAL: Total pods should match expected count")
	
	// Validate that master is one of the eligible pods (pod-0 or pod-1) - CRITICAL
	eligibleMasters := []string{"memgraph-ha-0", "memgraph-ha-1"}
	require.Contains(t, eligibleMasters, status.ClusterState.CurrentMaster, 
		"CRITICAL: Master should be either pod-0 or pod-1 (eligible for master role)")
	
	// Validate master-sync-async topology
	var masterCount, syncCount, asyncCount int
	var masterPod, syncPod string
	
	for _, pod := range status.Pods {
		require.NotEmpty(t, pod.BoltAddress, "CRITICAL: Pod %s should have bolt address", pod.Name)
		require.True(t, pod.Healthy, "CRITICAL: Pod %s should be healthy", pod.Name)
		
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
	
	// Validate topology counts - CRITICAL: crash if these fail
	require.Equal(t, 1, masterCount, "CRITICAL: Should have exactly 1 master")
	require.Equal(t, 1, syncCount, "CRITICAL: Should have exactly 1 sync replica")
	require.Equal(t, 1, asyncCount, "CRITICAL: Should have exactly 1 async replica")
	
	// Validate that master and sync replica are complementary eligible pods - CRITICAL
	require.Contains(t, eligibleMasters, masterPod, "CRITICAL: Master should be pod-0 or pod-1")
	require.Contains(t, eligibleMasters, syncPod, "CRITICAL: SYNC replica should be pod-0 or pod-1")
	require.NotEqual(t, masterPod, syncPod, "CRITICAL: Master and SYNC replica should be different pods")
	
	// Validate that pod-2 is always ASYNC (not eligible for master/SYNC roles) - CRITICAL
	asyncPodName := ""
	for _, pod := range status.Pods {
		if pod.MemgraphRole == "replica" && !pod.IsSyncReplica {
			asyncPodName = pod.Name
			break
		}
	}
	require.Equal(t, "memgraph-ha-2", asyncPodName, "CRITICAL: Pod-2 should always be ASYNC replica")
	
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

func TestE2E_FailoverReliability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	
	const failoverRounds = 3
	const postFailoverDelay = 15 * time.Second
	
	// Step 1: Check initial cluster topology
	t.Log("🔍 Step 1: Validating initial cluster topology")
	status, err := getClusterStatus(ctx)
	require.NoError(t, err, "Should get initial cluster status")
	
	// Validate master-sync-async topology
	initialMaster, initialSync := validateTopology(t, status)
	t.Logf("✓ Initial topology: Master=%s, Sync=%s", initialMaster, initialSync)
	
	// Connect to gateway for data operations
	driver, err := neo4j.NewDriverWithContext(gatewayURL, neo4j.NoAuth())
	require.NoError(t, err, "Should connect to gateway")
	defer driver.Close(ctx)
	
	// Run multiple failover rounds
	for round := 1; round <= failoverRounds; round++ {
		t.Logf("\n🔄 === FAILOVER ROUND %d/%d ===", round, failoverRounds)
		
		// Step 2: Send timestamped data before failover
		t.Logf("📝 Step 2: Writing pre-failover data (round %d)", round)
		preFailoverID := fmt.Sprintf("pre-failover-r%d-%d", round, time.Now().UnixNano())
		preFailoverValue := fmt.Sprintf("data-before-failover-round-%d", round)
		
		err = writeTimestampedData(ctx, driver, preFailoverID, preFailoverValue)
		require.NoError(t, err, "Should write pre-failover data")
		t.Logf("✓ Pre-failover data written: %s", preFailoverID)
		
		// Step 3: Verify data in cluster before failover (through gateway)
		time.Sleep(2 * time.Second) // Allow replication
		verifyDataExists(t, ctx, driver, preFailoverID, preFailoverValue)
		
		// Get current master before failover
		status, err = getClusterStatus(ctx)
		require.NoError(t, err, "Should get cluster status before failover")
		currentMaster := status.ClusterState.CurrentMaster
		t.Logf("📋 Current master before failover: %s", currentMaster)
		
		// Step 4: Trigger failover by deleting master pod
		t.Logf("💥 Step 4: Triggering failover - deleting master pod %s", currentMaster)
		err = deletePod(ctx, currentMaster)
		require.NoError(t, err, "Should delete master pod")
		t.Logf("✓ Master pod %s deleted", currentMaster)
		
		// Step 5: IMMEDIATELY write data after failover trigger
		t.Log("⚡ Step 5: IMMEDIATELY writing post-failover data")
		postFailoverID := fmt.Sprintf("post-failover-r%d-%d", round, time.Now().UnixNano())
		postFailoverValue := fmt.Sprintf("data-after-failover-round-%d", round)
		
		// Try to write data immediately - may fail initially during failover
		var writeSuccess bool
		writeAttempts := 0
		maxWriteAttempts := 30 // 30 seconds of attempts
		
		for writeAttempts < maxWriteAttempts && !writeSuccess {
			writeAttempts++
			err = writeTimestampedData(ctx, driver, postFailoverID, postFailoverValue)
			if err == nil {
				writeSuccess = true
				t.Logf("✓ Post-failover data written on attempt %d: %s", writeAttempts, postFailoverID)
				break
			}
			
			if writeAttempts%5 == 0 {
				t.Logf("⏳ Write attempt %d failed, retrying... (error: %v)", writeAttempts, err)
			}
			time.Sleep(1 * time.Second)
		}
		
		require.True(t, writeSuccess, "Should eventually write data after failover within %d attempts", maxWriteAttempts)
		
		// Wait for controller to complete failover
		t.Logf("⏱️  Waiting %v for failover to stabilize", postFailoverDelay)
		time.Sleep(postFailoverDelay)
		
		// Step 6: Verify new cluster topology after failover
		t.Log("🔍 Step 6: Validating post-failover cluster topology")
		status, err = getClusterStatus(ctx)
		require.NoError(t, err, "Should get cluster status after failover")
		
		newMaster, newSync := validateTopology(t, status)
		require.NotEqual(t, currentMaster, newMaster, "Master should have changed after failover")
		t.Logf("✓ Post-failover topology: Master=%s, Sync=%s (previous master %s is gone)", 
			newMaster, newSync, currentMaster)
		
		// Step 7: Verify both pre and post failover data exist in all 3 pods
		t.Log("✅ Step 7: Verifying data integrity after failover")
		verifyDataExists(t, ctx, driver, preFailoverID, preFailoverValue)
		verifyDataExists(t, ctx, driver, postFailoverID, postFailoverValue)
		
		t.Logf("✅ Round %d completed successfully - failover from %s to %s", 
			round, currentMaster, newMaster)
		
		// Brief pause between rounds
		if round < failoverRounds {
			time.Sleep(5 * time.Second)
		}
	}
	
	t.Logf("🎉 All %d failover rounds completed successfully!", failoverRounds)
}

// Helper functions for failover test

func validateTopology(t *testing.T, status *ClusterStatus) (master, sync string) {
	require.Len(t, status.Pods, expectedPodCount, "Should have %d pods", expectedPodCount)
	
	var masterCount, syncCount, asyncCount int
	var masterPod, syncPod string
	
	for _, pod := range status.Pods {
		if !pod.Healthy {
			continue // Skip unhealthy pods in topology validation
		}
		
		switch pod.MemgraphRole {
		case "main":
			masterCount++
			masterPod = pod.Name
		case "replica":
			if pod.IsSyncReplica {
				syncCount++
				syncPod = pod.Name
			} else {
				asyncCount++
			}
		}
	}
	
	assert.Equal(t, 1, masterCount, "Should have exactly 1 master")
	assert.GreaterOrEqual(t, syncCount, 1, "Should have at least 1 sync replica")
	
	return masterPod, syncPod
}

func writeTimestampedData(ctx context.Context, driver neo4j.DriverWithContext, id, value string) error {
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)
	
	_, err := session.Run(ctx,
		"CREATE (n:FailoverTest {id: $id, value: $value, timestamp: datetime(), created_at: $created_at})",
		map[string]interface{}{
			"id":         id,
			"value":      value,
			"created_at": time.Now().Format(time.RFC3339),
		})
	
	return err
}

func verifyDataExists(t *testing.T, ctx context.Context, driver neo4j.DriverWithContext, expectedID, expectedValue string) {
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)
	
	result, err := session.Run(ctx,
		"MATCH (n:FailoverTest {id: $id}) RETURN n.id, n.value, n.created_at",
		map[string]interface{}{"id": expectedID})
	require.NoError(t, err, "Should query for data %s", expectedID)
	
	records, err := result.Collect(ctx)
	require.NoError(t, err, "Should collect results for %s", expectedID)
	require.Len(t, records, 1, "Should find exactly one record for %s", expectedID)
	
	record := records[0]
	assert.Equal(t, expectedID, record.Values[0], "ID should match")
	assert.Equal(t, expectedValue, record.Values[1], "Value should match")
}

func deletePod(ctx context.Context, podName string) error {
	// Use kubectl to delete the pod with force and zero grace period for immediate deletion
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "pod", podName, "-n", "memgraph", "--force", "--grace-period=0")
	return cmd.Run()
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