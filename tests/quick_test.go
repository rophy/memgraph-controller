package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_ClusterTopology validates the cluster has the expected topology
func TestE2E_ClusterTopology(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// Wait for controller to be ready
	err := waitForController(ctx)
	require.NoError(t, err, "Controller should be reachable")

	// Get cluster status
	status, err := getClusterStatus(ctx)
	require.NoError(t, err, "Should get cluster status")

	// Verify we have exactly 3 pods
	require.Len(t, status.Pods, expectedPodCount, "Should have exactly %d pods", expectedPodCount)

	// Verify each pod has correct state
	var mainCount, syncCount, asyncCount int
	var mainPod, syncPod string
	eligibleMains := []string{"memgraph-ha-0", "memgraph-ha-1"}

	for _, pod := range status.Pods {
		assert.True(t, pod.Healthy, "Pod %s should be healthy", pod.Name)
		assert.NotEmpty(t, pod.IPAddress, "Pod %s should have IP address", pod.Name)

		switch pod.MemgraphRole {
		case "main":
			mainCount++
			mainPod = pod.Name
		case "replica":
			if isReplicaSyncMode(pod.Name, status.ClusterState.ReplicaRegistrations) {
				syncCount++
				syncPod = pod.Name
			} else {
				asyncCount++
			}
		}
	}

	// Validate topology counts - CRITICAL: crash if these fail
	require.Equal(t, 1, mainCount, "CRITICAL: Should have exactly 1 main")
	require.Equal(t, 1, syncCount, "CRITICAL: Should have exactly 1 sync replica")
	require.Equal(t, 1, asyncCount, "CRITICAL: Should have exactly 1 async replica")

	// Validate that main and sync replica are complementary eligible pods - CRITICAL
	require.Contains(t, eligibleMains, mainPod, "CRITICAL: Main should be pod-0 or pod-1")
	require.Contains(t, eligibleMains, syncPod, "CRITICAL: SYNC replica should be pod-0 or pod-1")
	require.NotEqual(t, mainPod, syncPod, "CRITICAL: Main and SYNC replica should be different pods")

	// Validate that pod-2 is always ASYNC (not eligible for main/SYNC roles) - CRITICAL
	asyncPodName := ""
	for _, pod := range status.Pods {
		if pod.MemgraphRole == "replica" && !isReplicaSyncMode(pod.Name, status.ClusterState.ReplicaRegistrations) {
			asyncPodName = pod.Name
			break
		}
	}
	require.Equal(t, "memgraph-ha-2", asyncPodName, "CRITICAL: Pod-2 should always be ASYNC replica")

	t.Logf("✓ Cluster topology validated: Main=%s, Sync=%s, Total pods=%d",
		mainPod, syncPod, len(status.Pods))
}

// TestE2E_DataWriteThoughGateway verifies that data can be written through the gateway
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

	// Write test data using ExecuteWrite for automatic retry
	_, err = session.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		_, err := tx.Run(ctx,
			"CREATE (n:TestNode {id: $id, value: $value, timestamp: $ts}) RETURN n.id",
			map[string]interface{}{
				"id":    testID,
				"value": testValue,
				"ts":    time.Now().Unix(),
			})
		return nil, err
	})
	require.NoError(t, err, "Should write data through gateway")

	// Verify data was written by reading it back using ExecuteRead for automatic retry
	records, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		result, err := tx.Run(ctx,
			"MATCH (n:TestNode {id: $id}) RETURN n.id, n.value, n.timestamp",
			map[string]interface{}{"id": testID})
		if err != nil {
			return nil, err
		}
		return result.Collect(ctx)
	})
	require.NoError(t, err, "Should read data through gateway")

	// Validate the written data
	recordList := records.([]*neo4j.Record)
	require.Len(t, recordList, 1, "Should find exactly one test record")

	record := recordList[0]
	assert.Equal(t, testID, record.Values[0], "ID should match")
	assert.Equal(t, testValue, record.Values[1], "Value should match")

	t.Logf("✓ Data write validated: ID=%s, Value=%s", testID, testValue)
}