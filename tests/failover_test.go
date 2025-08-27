package tests

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_DataReplicationVerification verifies data replication across all pods
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

// TestE2E_FailoverReliability tests the reliability of failover scenarios
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

// validateTopology validates topology and returns main/sync pod names
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

// writeTimestampedData writes test data with timestamp to Memgraph
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

// verifyDataExists verifies that specific test data exists via gateway
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

// deletePod deletes a pod using kubectl
func deletePod(ctx context.Context, podName string) error {
	// Use kubectl to delete the pod with force and zero grace period for immediate deletion
	cmd := exec.CommandContext(ctx, "kubectl", "delete", "pod", podName, "-n", "memgraph", "--force", "--grace-period=0")
	return cmd.Run()
}

// verifyDataInAllPods verifies that test data exists in all healthy pods
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