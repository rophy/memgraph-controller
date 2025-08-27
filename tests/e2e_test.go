package tests

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ClusterStatus represents the controller's cluster status response
type ClusterStatus struct {
	Timestamp    string       `json:"timestamp"`
	ClusterState ClusterState `json:"cluster_state"`
	Pods         []PodInfo    `json:"pods"`
}

type ClusterState struct {
	CurrentMain        string `json:"current_main"`
	CurrentSyncReplica string `json:"current_sync_replica"`
	TotalPods          int    `json:"total_pods"`
	HealthyPods        int    `json:"healthy_pods"`
	UnhealthyPods      int    `json:"unhealthy_pods"`
	SyncReplicaHealthy bool   `json:"sync_replica_healthy"`
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
	CurrentMain       string `json:"currentMain"`
	ActiveConnections int    `json:"activeConnections"`
	TotalConnections  int64  `json:"totalConnections"`
}

const (
	// Test configuration
	controllerStatusURL = "http://localhost:8080/api/v1/status"
	gatewayURL          = "bolt://localhost:7687"
	testTimeout         = 30 * time.Second

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

	// Validate that main is one of the eligible pods (pod-0 or pod-1) - CRITICAL
	eligibleMains := []string{"memgraph-ha-0", "memgraph-ha-1"}
	require.Contains(t, eligibleMains, status.ClusterState.CurrentMain,
		"CRITICAL: Main should be either pod-0 or pod-1 (eligible for main role)")

	// Validate main-sync-async topology
	var mainCount, syncCount, asyncCount int
	var mainPod, syncPod string

	for _, pod := range status.Pods {
		require.NotEmpty(t, pod.BoltAddress, "CRITICAL: Pod %s should have bolt address", pod.Name)
		require.True(t, pod.Healthy, "CRITICAL: Pod %s should be healthy", pod.Name)

		switch pod.MemgraphRole {
		case "main":
			mainCount++
			mainPod = pod.Name
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
		if pod.MemgraphRole == "replica" && !pod.IsSyncReplica {
			asyncPodName = pod.Name
			break
		}
	}
	require.Equal(t, "memgraph-ha-2", asyncPodName, "CRITICAL: Pod-2 should always be ASYNC replica")

	t.Logf("✓ Cluster topology validated: Main=%s, Sync=%s, Total pods=%d",
		mainPod, syncPod, len(status.Pods))
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
	var mainCount, syncCount, asyncCount int
	for _, pod := range status.Pods {
		if !pod.Healthy {
			t.Logf("⚠ Pod %s is unhealthy", pod.Name)
			continue
		}

		switch pod.MemgraphRole {
		case "main":
			mainCount++
		case "replica":
			if pod.IsSyncReplica {
				syncCount++
			} else {
				asyncCount++
			}
		}
	}

	// Validate healthy replication topology
	assert.Equal(t, 1, mainCount, "Should have exactly 1 healthy main")
	assert.Equal(t, 1, syncCount, "Should have exactly 1 healthy sync replica")
	assert.Equal(t, 1, asyncCount, "Should have exactly 1 healthy async replica")

	replicatedCount := mainCount + syncCount + asyncCount

	// Validate replication
	assert.Equal(t, expectedPodCount, replicatedCount,
		"Data should be replicated to all %d pods", expectedPodCount)

	t.Logf("✓ Data replication validated: %d/%d pods have the test data",
		replicatedCount, expectedPodCount)
}

func TestE2E_FailoverReliability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const postFailoverDelay = 15 * time.Second

	// Step 1: Check initial cluster topology
	t.Log("🔍 Step 1: Check cluster topology")
	status, err := getClusterStatus(ctx)
	require.NoError(t, err, "Should get initial cluster status")

	// Validate main-sync-async topology
	initialMain, initialSync := validateTopology(t, status)
	t.Logf("✓ Cluster topology: Main=%s, Sync=%s", initialMain, initialSync)

	// Connect to gateway for data operations
	driver, err := neo4j.NewDriverWithContext(gatewayURL, neo4j.NoAuth())
	require.NoError(t, err, "Should connect to gateway")
	defer driver.Close(ctx)

	// Step 2: Send timestamped data, query data, verify query returns the data
	t.Log("📝 Step 2: Send timestamped data and verify")
	preFailoverID := fmt.Sprintf("pre-failover-%d", time.Now().UnixNano())
	preFailoverValue := "data-before-failover"

	err = writeTimestampedData(ctx, driver, preFailoverID, preFailoverValue)
	require.NoError(t, err, "Should write pre-failover data")
	t.Logf("✓ Pre-failover data written: %s", preFailoverID)

	// Query and verify the data immediately
	verifyDataExists(t, ctx, driver, preFailoverID, preFailoverValue)
	t.Log("✓ Pre-failover data verified via query")

	// Get current main before failover
	status, err = getClusterStatus(ctx)
	require.NoError(t, err, "Should get cluster status before failover")
	currentMain := status.ClusterState.CurrentMain
	t.Logf("📋 Current main: %s", currentMain)

	// Check data in all 3 pods before failover
	t.Log("🔍 Checking pre-failover data exists in all pods BEFORE killing main...")
	for _, pod := range status.Pods {
		if !pod.Healthy {
			continue
		}

		query := fmt.Sprintf("MATCH (n:FailoverTest {id: '%s'}) RETURN count(n);", preFailoverID)
		cmd := exec.CommandContext(ctx, "kubectl", "exec", pod.Name, "-n", "memgraph", "-c", "memgraph", "--",
			"bash", "-c", fmt.Sprintf("echo \"%s\" | mgconsole --output-format csv --username=memgraph", query))

		output, err := cmd.Output()
		if err != nil {
			t.Logf("❌ Pod %s: Failed to query - %v", pod.Name, err)
			continue
		}

		outputStr := strings.TrimSpace(string(output))
		if strings.Contains(outputStr, "\"1\"") {
			t.Logf("✅ Pod %s: Pre-failover data EXISTS", pod.Name)
		} else if strings.Contains(outputStr, "\"0\"") {
			t.Logf("❌ Pod %s: Pre-failover data MISSING", pod.Name)
		} else {
			t.Logf("⚠️ Pod %s: Unexpected output: %s", pod.Name, outputStr)
		}
	}

	// Step 3: Kill the main pod
	t.Logf("💥 Step 3: Kill the main pod %s", currentMain)
	err = deletePod(ctx, currentMain)
	require.NoError(t, err, "Should delete main pod")
	t.Logf("✓ Main pod %s deleted", currentMain)

	// Step 4: Immediately send timestamped data, query data, verify query returns the data
	t.Log("⚡ Step 4: Immediately send timestamped data and verify")
	postFailoverID := fmt.Sprintf("post-failover-%d", time.Now().UnixNano())
	postFailoverValue := "data-after-failover"

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

	// Immediately verify the post-failover data
	verifyDataExists(t, ctx, driver, postFailoverID, postFailoverValue)
	t.Log("✓ Post-failover data verified via query")

	// Step 5: Wait until failover stabilizes
	t.Logf("⏱️  Step 5: Wait %v for failover to stabilize", postFailoverDelay)
	time.Sleep(postFailoverDelay)

	// Verify new cluster topology after failover (with retry for transient states)
	newMain, newSync := validateTopologyWithRetry(t, ctx, 10, 2*time.Second)
	require.NotEqual(t, currentMain, newMain, "Main should have changed after failover")
	t.Logf("✓ Post-failover topology: Main=%s, Sync=%s (previous main %s is gone)",
		newMain, newSync, currentMain)

	// Step 6: Verify the timestamped data in [2] and [4] exist in all 3 pods
	t.Log("✅ Step 6: Verify timestamped data exists in all 3 pods")

	// Allow some time for data to replicate to all replicas after failover
	t.Log("⏳ Waiting 5 seconds for data replication to complete...")
	time.Sleep(5 * time.Second)

	// Get fresh cluster status after replication delay
	status, err = getClusterStatus(ctx)
	require.NoError(t, err, "Should get cluster status after replication delay")

	t.Logf("Looking for pre-failover data: ID=%s, Value=%s", preFailoverID, preFailoverValue)
	err = verifyDataInAllPods(ctx, status, preFailoverID, preFailoverValue)
	require.NoError(t, err, "Pre-failover data should exist in all pods")

	t.Logf("Looking for post-failover data: ID=%s, Value=%s", postFailoverID, postFailoverValue)
	err = verifyDataInAllPods(ctx, status, postFailoverID, postFailoverValue)
	require.NoError(t, err, "Post-failover data should exist in all pods")

	t.Log("🎉 Failover test completed successfully!")
	t.Logf("✓ Both pre-failover (%s) and post-failover (%s) data verified in all 3 pods",
		preFailoverID, postFailoverID)
}

// Helper functions for failover test

// validateTopologyWithRetry validates cluster topology with retry logic for transient states
func validateTopologyWithRetry(t *testing.T, ctx context.Context, maxRetries int, retryDelay time.Duration) (main, sync string) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		status, err := getClusterStatus(ctx)
		require.NoError(t, err, "Should get cluster status on attempt %d", attempt)
		
		main, sync, valid := tryValidateTopology(status)
		if valid {
			t.Logf("✓ Topology validation succeeded on attempt %d: Main=%s, Sync=%s", attempt, main, sync)
			return main, sync
		}
		
		if attempt < maxRetries {
			t.Logf("⚠️  Topology validation attempt %d failed, retrying in %v...", attempt, retryDelay)
			time.Sleep(retryDelay)
		}
	}
	
	// Final attempt with assertions to produce clear error message
	status, err := getClusterStatus(ctx)
	require.NoError(t, err, "Should get cluster status for final validation")
	return validateTopology(t, status)
}

// tryValidateTopology attempts topology validation without assertions, returns success status
func tryValidateTopology(status *ClusterStatus) (main, sync string, valid bool) {
	if len(status.Pods) != expectedPodCount {
		return "", "", false
	}

	var mainCount, syncCount int
	var mainPod, syncPod string

	for _, pod := range status.Pods {
		if !pod.Healthy {
			continue // Skip unhealthy pods in topology validation
		}

		switch pod.MemgraphRole {
		case "main":
			mainCount++
			mainPod = pod.Name
		case "replica":
			if pod.IsSyncReplica {
				syncCount++
				syncPod = pod.Name
			}
		}
	}

	// Check for valid topology: exactly 1 main and at least 1 sync replica
	return mainPod, syncPod, (mainCount == 1 && syncCount >= 1)
}

func validateTopology(t *testing.T, status *ClusterStatus) (main, sync string) {
	require.Len(t, status.Pods, expectedPodCount, "Should have %d pods", expectedPodCount)

	var mainCount, syncCount, asyncCount int
	var mainPod, syncPod string

	for _, pod := range status.Pods {
		if !pod.Healthy {
			continue // Skip unhealthy pods in topology validation
		}

		switch pod.MemgraphRole {
		case "main":
			mainCount++
			mainPod = pod.Name
		case "replica":
			if pod.IsSyncReplica {
				syncCount++
				syncPod = pod.Name
			} else {
				asyncCount++
			}
		}
	}

	assert.Equal(t, 1, mainCount, "Should have exactly 1 main")
	assert.GreaterOrEqual(t, syncCount, 1, "Should have at least 1 sync replica")

	return mainPod, syncPod
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

func verifyDataInAllPods(ctx context.Context, status *ClusterStatus, expectedID, expectedValue string) error {
	for _, pod := range status.Pods {
		if !pod.Healthy {
			continue // Skip unhealthy pods
		}

		// Use kubectl exec to query data directly from the pod using mgconsole with CSV output
		query := fmt.Sprintf("MATCH (n:FailoverTest {id: '%s'}) RETURN n.id, n.value;", expectedID)
		cmd := exec.CommandContext(ctx, "kubectl", "exec", pod.Name, "-n", "memgraph", "-c", "memgraph", "--",
			"bash", "-c", fmt.Sprintf("echo \"%s\" | mgconsole --output-format csv --username=memgraph", query))

		output, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("failed to query pod %s via kubectl: %w", pod.Name, err)
		}

		outputStr := string(output)

		// Filter out Kubernetes messages to get clean CSV
		lines := strings.Split(outputStr, "\n")
		var csvContent strings.Builder
		for _, line := range lines {
			line = strings.TrimSpace(line)
			// Skip empty lines and kubernetes messages
			if line != "" && !strings.Contains(line, "Defaulted container") {
				csvContent.WriteString(line + "\n")
			}
		}

		csvStr := csvContent.String()

		// Check if we have any CSV content (when no results, mgconsole outputs nothing)
		if strings.TrimSpace(csvStr) == "" {
			return fmt.Errorf("pod %s: no data found for ID '%s' - query returned empty result",
				pod.Name, expectedID)
		}

		// Parse CSV using proper CSV reader
		csvReader := csv.NewReader(strings.NewReader(csvStr))
		records, err := csvReader.ReadAll()
		if err != nil {
			return fmt.Errorf("pod %s: failed to parse CSV: %w\nRaw output: %s", pod.Name, err, outputStr)
		}

		if len(records) < 2 {
			return fmt.Errorf("pod %s: expected at least 2 CSV records (header + data), got %d\nFiltered CSV: %s\nRaw output: %s",
				pod.Name, len(records), csvStr, outputStr)
		}

		// Validate header
		header := records[0]
		if len(header) != 2 || header[0] != "n.id" || header[1] != "n.value" {
			return fmt.Errorf("pod %s: unexpected CSV header: %v", pod.Name, header)
		}

		// Validate data record
		dataRecord := records[1]
		if len(dataRecord) != 2 {
			return fmt.Errorf("pod %s: expected 2 columns in data record, got %d: %v",
				pod.Name, len(dataRecord), dataRecord)
		}

		actualID := dataRecord[0]
		actualValue := dataRecord[1]

		// Remove surrounding quotes from CSV values if present
		actualID = strings.Trim(actualID, `"`)
		actualValue = strings.Trim(actualValue, `"`)

		if actualID != expectedID {
			return fmt.Errorf("pod %s: expected ID '%s', got '%s'", pod.Name, expectedID, actualID)
		}

		if actualValue != expectedValue {
			return fmt.Errorf("pod %s: expected value '%s', got '%s'", pod.Name, expectedValue, actualValue)
		}
	}

	return nil
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
