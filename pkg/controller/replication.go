package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// ConfigureReplication configures replication for the cluster
func (c *MemgraphController) ConfigureReplication(ctx context.Context, clusterState *ClusterState) error {
	if len(clusterState.Pods) == 0 {
		log.Println("No pods to configure replication for")
		return nil
	}

	log.Println("Starting replication configuration...")
	currentMain := clusterState.CurrentMain

	if currentMain == "" {
		return fmt.Errorf("no main pod selected for replication configuration")
	}

	log.Printf("Configuring replication with main: %s", currentMain)

	// Phase 1: Configure pod roles (MAIN/REPLICA)
	var configErrors []error

	for podName, podInfo := range clusterState.Pods {
		if !podInfo.NeedsReplicationConfiguration(currentMain) {
			log.Printf("Pod %s already in correct replication state", podName)
			continue
		}

		if podInfo.ShouldBecomeMain(currentMain) {
			// Check if pod is healthy before attempting promotion
			if !c.isPodHealthyForMain(podInfo) {
				log.Printf("Cannot promote unhealthy pod %s to MAIN (likely deleted or terminating)", podName)
				configErrors = append(configErrors, fmt.Errorf("promote %s to MAIN: pod is unhealthy", podName))
				continue
			}

			log.Printf("Promoting pod %s to MAIN role", podName)
			if err := c.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, podInfo.BoltAddress); err != nil {
				log.Printf("Failed to promote pod %s to MAIN: %v", podName, err)
				configErrors = append(configErrors, fmt.Errorf("promote %s to MAIN: %w", podName, err))
				continue
			}
			log.Printf("Successfully promoted pod %s to MAIN", podName)
		}

		if podInfo.ShouldBecomeReplica(currentMain) {
			// Check if pod is healthy before attempting demotion
			if !c.isPodHealthyForMain(podInfo) {
				log.Printf("Skipping demotion of unhealthy pod %s (likely deleted or terminating)", podName)
				continue
			}

			log.Printf("Demoting pod %s to REPLICA role", podName)
			if err := c.memgraphClient.SetReplicationRoleToReplicaWithRetry(ctx, podInfo.BoltAddress); err != nil {
				log.Printf("Failed to demote pod %s to REPLICA: %v", podName, err)
				configErrors = append(configErrors, fmt.Errorf("demote %s to REPLICA: %w", podName, err))
				continue
			}
			log.Printf("Successfully demoted pod %s to REPLICA", podName)
		}
	}

	// Phase 2: Configure SYNC/ASYNC replication strategy with controller state authority
	if err := c.configureReplicationWithEnhancedSyncStrategy(ctx, clusterState); err != nil {
		log.Printf("Failed to configure enhanced SYNC/ASYNC replication: %v", err)
		configErrors = append(configErrors, fmt.Errorf("enhanced SYNC/ASYNC replication configuration: %w", err))
	}

	// Phase 2.5: Monitor SYNC replica health and handle failures
	if err := c.detectSyncReplicaHealth(ctx, clusterState); err != nil {
		log.Printf("SYNC replica health check failed: %v", err)
		configErrors = append(configErrors, fmt.Errorf("SYNC replica health monitoring: %w", err))
	}

	// Phase 3: Handle any existing replicas that should be removed
	// TEMPORARILY DISABLED: Conservative cleanup to prevent state corruption
	log.Printf("Skipping replica cleanup phase to preserve existing replication state")
	// if err := c.cleanupObsoleteReplicas(ctx, clusterState); err != nil {
	//	log.Printf("Warning: failed to cleanup obsolete replicas: %v", err)
	//	configErrors = append(configErrors, fmt.Errorf("cleanup obsolete replicas: %w", err))
	// }

	if len(configErrors) > 0 {
		log.Printf("Replication configuration completed with %d errors:", len(configErrors))
		for _, err := range configErrors {
			log.Printf("  - %v", err)
		}
		return fmt.Errorf("replication configuration had %d errors (see logs for details)", len(configErrors))
	}

	log.Printf("Replication configuration completed successfully for %d pods", len(clusterState.Pods))
	return nil
}

// selectSyncReplica determines which replica should be configured as SYNC using controller state authority
func (c *MemgraphController) selectSyncReplica(clusterState *ClusterState, currentMain string) string {
	log.Printf("Selecting SYNC replica using deterministic controller strategy...")

	// Strategy: Use controller's two-pod main/SYNC strategy
	// If pod-0 is main, pod-1 is SYNC replica (and vice versa)
	currentMainIndex := c.config.ExtractPodIndex(currentMain)

	if currentMainIndex == 0 {
		// Main is pod-0, SYNC replica should be pod-1
		syncReplicaName := c.config.GetPodName(1)
		if _, exists := clusterState.Pods[syncReplicaName]; exists {
			log.Printf("Deterministic SYNC selection: %s (main=pod-0)", syncReplicaName)
			return syncReplicaName
		}
	} else if currentMainIndex == 1 {
		// Main is pod-1, SYNC replica should be pod-0
		syncReplicaName := c.config.GetPodName(0)
		if _, exists := clusterState.Pods[syncReplicaName]; exists {
			log.Printf("Deterministic SYNC selection: %s (main=pod-1)", syncReplicaName)
			return syncReplicaName
		}
	}

	// Fallback: main is not pod-0 or pod-1, or target SYNC replica doesn't exist
	// Use alphabetical selection as fallback
	log.Printf("Fallback SYNC selection: main index=%d not in eligible range", currentMainIndex)
	return c.selectSyncReplicaFallback(clusterState, currentMain)
}

// selectSyncReplicaFallback uses alphabetical selection as fallback
func (c *MemgraphController) selectSyncReplicaFallback(clusterState *ClusterState, currentMain string) string {
	var eligibleReplicas []string

	// Collect all non-main pods as potential SYNC replicas
	for podName := range clusterState.Pods {
		if podName != currentMain {
			eligibleReplicas = append(eligibleReplicas, podName)
		}
	}

	if len(eligibleReplicas) == 0 {
		return "" // No replicas available
	}

	// Sort alphabetically to ensure deterministic selection
	for i := 0; i < len(eligibleReplicas)-1; i++ {
		for j := i + 1; j < len(eligibleReplicas); j++ {
			if eligibleReplicas[i] > eligibleReplicas[j] {
				eligibleReplicas[i], eligibleReplicas[j] = eligibleReplicas[j], eligibleReplicas[i]
			}
		}
	}

	log.Printf("Fallback SYNC replica selected: %s (alphabetical first)", eligibleReplicas[0])
	return eligibleReplicas[0]
}

// identifySyncReplica finds which replica (if any) is currently configured as SYNC
func (c *MemgraphController) identifySyncReplica(clusterState *ClusterState, currentMain string) string {
	mainPod, exists := clusterState.Pods[currentMain]
	if !exists {
		return ""
	}

	// Check ReplicasInfo for any SYNC replica
	for _, replica := range mainPod.ReplicasInfo {
		if replica.SyncMode == "sync" { // Memgraph returns lowercase "sync"
			// Convert replica name back to pod name (underscores to dashes)
			podName := strings.ReplaceAll(replica.Name, "_", "-")
			return podName
		}
	}

	return "" // No SYNC replica found
}

// configureReplicationWithEnhancedSyncStrategy configures replication using enhanced SYNC/ASYNC strategy with controller authority
func (c *MemgraphController) configureReplicationWithEnhancedSyncStrategy(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Configuring enhanced SYNC/ASYNC replication with controller state authority...")

	currentMain := clusterState.CurrentMain
	if currentMain == "" {
		return fmt.Errorf("no main pod selected for enhanced SYNC replication configuration")
	}

	_, exists := clusterState.Pods[currentMain]
	if !exists {
		return fmt.Errorf("main pod %s not found in cluster state", currentMain)
	}

	// Use controller state authority to determine SYNC replica
	targetSyncReplica := c.selectSyncReplica(clusterState, currentMain)
	currentSyncReplica := c.identifySyncReplica(clusterState, currentMain)

	log.Printf("Enhanced SYNC strategy: target=%s, current=%s, main=%s",
		targetSyncReplica, currentSyncReplica, currentMain)

	var configErrors []error

	// Phase 1: Ensure SYNC replica is configured correctly
	if err := c.ensureCorrectSyncReplica(ctx, clusterState, targetSyncReplica, currentSyncReplica); err != nil {
		configErrors = append(configErrors, fmt.Errorf("SYNC replica configuration: %w", err))
	}

	// Phase 2: Configure remaining pods as ASYNC replicas
	if err := c.configureAsyncReplicas(ctx, clusterState, targetSyncReplica); err != nil {
		configErrors = append(configErrors, fmt.Errorf("ASYNC replica configuration: %w", err))
	}

	// Phase 3: Verify final SYNC replica configuration
	if err := c.verifySyncReplicaConfiguration(ctx, clusterState); err != nil {
		log.Printf("⚠️  SYNC replica verification failed: %v", err)
		configErrors = append(configErrors, fmt.Errorf("SYNC replica verification: %w", err))
	}

	if len(configErrors) > 0 {
		log.Printf("Enhanced SYNC replication configuration completed with %d errors:", len(configErrors))
		for _, err := range configErrors {
			log.Printf("  - %v", err)
		}
		// Continue despite errors - partial configuration is better than none
	}

	log.Printf("✅ Enhanced SYNC replication configuration completed")
	return nil
}

// ensureCorrectSyncReplica ensures the correct pod is configured as SYNC replica
func (c *MemgraphController) ensureCorrectSyncReplica(ctx context.Context, clusterState *ClusterState, targetSyncReplica, currentSyncReplica string) error {
	if targetSyncReplica == "" {
		log.Printf("No target SYNC replica available - skipping SYNC configuration")
		return nil
	}

	// If target matches current, verify it's correctly configured
	if targetSyncReplica == currentSyncReplica {
		log.Printf("SYNC replica %s already correctly assigned", targetSyncReplica)
		return c.verifyPodSyncConfiguration(ctx, clusterState, targetSyncReplica)
	}

	// Need to change SYNC replica assignment
	log.Printf("Changing SYNC replica: %s → %s", currentSyncReplica, targetSyncReplica)

	// Step 1: Remove old SYNC replica if it exists
	if currentSyncReplica != "" {
		if err := c.demoteSyncToAsync(ctx, clusterState, currentSyncReplica); err != nil {
			log.Printf("Warning: Failed to demote old SYNC replica %s: %v", currentSyncReplica, err)
		}
	}

	// Step 2: Configure new SYNC replica
	if err := c.configurePodAsSyncReplica(ctx, clusterState, targetSyncReplica); err != nil {
		return fmt.Errorf("failed to configure new SYNC replica %s: %w", targetSyncReplica, err)
	}

	log.Printf("✅ SYNC replica successfully changed to %s", targetSyncReplica)
	return nil
}

// configureAsyncReplicas configures all non-main, non-SYNC pods as ASYNC replicas
func (c *MemgraphController) configureAsyncReplicas(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) error {
	currentMain := clusterState.CurrentMain
	mainPod := clusterState.Pods[currentMain]
	var configErrors []error

	for podName, podInfo := range clusterState.Pods {
		// Skip main and SYNC replica
		if podName == currentMain || podName == syncReplicaPod {
			continue
		}

		// Configure as ASYNC replica
		if err := c.configurePodAsAsyncReplica(ctx, mainPod, podInfo); err != nil {
			log.Printf("Warning: Failed to configure ASYNC replica %s: %v", podName, err)
			configErrors = append(configErrors, fmt.Errorf("ASYNC replica %s: %w", podName, err))
			continue
		}

		log.Printf("✅ Configured %s as ASYNC replica", podName)
	}

	if len(configErrors) > 0 {
		return fmt.Errorf("ASYNC replica configuration had %d errors", len(configErrors))
	}

	return nil
}

// configurePodAsAsyncReplica configures a specific pod as ASYNC replica
func (c *MemgraphController) configurePodAsAsyncReplica(ctx context.Context, mainPod, replicaPod *PodInfo) error {
	replicaName := replicaPod.GetReplicaName()
	replicaAddress := replicaPod.GetReplicationAddress()

	// Check if replica pod is ready for replication
	if replicaAddress == "" || !replicaPod.IsReadyForReplication() {
		return fmt.Errorf("replica pod %s not ready for replication (IP: %s, Ready: %v)",
			replicaPod.Name, replicaAddress, replicaPod.IsReadyForReplication())
	}

	// Check if already configured correctly as ASYNC
	for _, replica := range mainPod.ReplicasInfo {
		if replica.Name == replicaName && replica.SyncMode == "async" {
			replicaPod.IsSyncReplica = false
			return nil // Already configured correctly
		}
	}

	// Need to register/re-register as ASYNC
	// Drop existing registration first (if any)
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, mainPod.BoltAddress, replicaName); err != nil {
		log.Printf("Warning: Could not drop existing replica %s: %v", replicaName, err)
	}

	// Register as ASYNC replica
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, mainPod.BoltAddress, replicaName, replicaAddress, "ASYNC"); err != nil {
		return fmt.Errorf("failed to register ASYNC replica %s: %w", replicaName, err)
	}

	// Update tracking
	replicaPod.IsSyncReplica = false
	return nil
}

// demoteSyncToAsync demotes a SYNC replica to ASYNC mode
func (c *MemgraphController) demoteSyncToAsync(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) error {
	log.Printf("Demoting SYNC replica %s to ASYNC mode", syncReplicaPod)

	syncPod, exists := clusterState.Pods[syncReplicaPod]
	if !exists {
		return fmt.Errorf("SYNC replica pod %s not found", syncReplicaPod)
	}

	mainPod := clusterState.Pods[clusterState.CurrentMain]
	replicaName := syncPod.GetReplicaName()
	replicaAddress := syncPod.GetReplicationAddress()

	// Check if replica pod is ready for replication
	if replicaAddress == "" || !syncPod.IsReadyForReplication() {
		return fmt.Errorf("replica pod %s not ready for replication (IP: %s, Ready: %v)",
			syncPod.Name, replicaAddress, syncPod.IsReadyForReplication())
	}

	// Drop SYNC registration
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, mainPod.BoltAddress, replicaName); err != nil {
		return fmt.Errorf("failed to drop SYNC replica %s: %w", replicaName, err)
	}

	// Re-register as ASYNC
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, mainPod.BoltAddress, replicaName, replicaAddress, "ASYNC"); err != nil {
		return fmt.Errorf("failed to re-register %s as ASYNC: %w", replicaName, err)
	}

	// Update tracking
	syncPod.IsSyncReplica = false
	log.Printf("Successfully demoted %s from SYNC to ASYNC", syncReplicaPod)

	return nil
}

// verifyPodSyncConfiguration verifies that a pod is correctly configured as SYNC replica
func (c *MemgraphController) verifyPodSyncConfiguration(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) error {
	syncPod := clusterState.Pods[syncReplicaPod]
	mainPod := clusterState.Pods[clusterState.CurrentMain]
	replicaName := syncPod.GetReplicaName()

	// Check if registered as SYNC with main
	for _, replica := range mainPod.ReplicasInfo {
		if replica.Name == replicaName {
			if replica.SyncMode == "sync" {
				syncPod.IsSyncReplica = true
				return nil
			}
			return fmt.Errorf("pod %s registered with sync_mode=%s, expected 'sync'", syncReplicaPod, replica.SyncMode)
		}
	}

	return fmt.Errorf("pod %s not registered as replica with main", syncReplicaPod)
}

// verifySyncReplicaConfiguration verifies the final SYNC replica configuration
func (c *MemgraphController) verifySyncReplicaConfiguration(ctx context.Context, clusterState *ClusterState) error {
	// Count SYNC replicas
	syncReplicaCount := 0
	var syncReplicaName string

	for podName, podInfo := range clusterState.Pods {
		if podInfo.IsSyncReplica {
			syncReplicaCount++
			syncReplicaName = podName
		}
	}

	// Validate SYNC replica count
	switch syncReplicaCount {
	case 0:
		log.Printf("⚠️  WARNING: No SYNC replica configured - cluster may have write availability issues")
		return fmt.Errorf("no SYNC replica configured")

	case 1:
		log.Printf("✅ SYNC replica verification passed: %s", syncReplicaName)
		return nil

	default:
		log.Printf("❌ ERROR: Multiple SYNC replicas detected (%d) - this should not happen", syncReplicaCount)
		return fmt.Errorf("multiple SYNC replicas detected: %d", syncReplicaCount)
	}
}

// detectSyncReplicaHealth monitors SYNC replica health and handles failures
func (c *MemgraphController) detectSyncReplicaHealth(ctx context.Context, clusterState *ClusterState) error {
	currentMain := clusterState.CurrentMain
	if currentMain == "" {
		return fmt.Errorf("no main available for SYNC replica health monitoring")
	}

	mainPod, exists := clusterState.Pods[currentMain]
	if !exists {
		return fmt.Errorf("main pod %s not found for SYNC replica health monitoring", currentMain)
	}

	// Find current SYNC replica
	var syncReplicaName string
	var syncReplicaInfo *ReplicaInfo
	for _, replica := range mainPod.ReplicasInfo {
		if replica.SyncMode == "sync" {
			syncReplicaName = strings.ReplaceAll(replica.Name, "_", "-")
			syncReplicaInfo = &replica
			break
		}
	}

	if syncReplicaName == "" {
		log.Printf("⚠️  No SYNC replica detected - cluster in degraded state")
		log.Printf("🚨 CRITICAL: Manual intervention required to restore SYNC replica")
		log.Printf("💡 Options:")
		log.Printf("  1. Restart existing SYNC replica pod if it exists")
		log.Printf("  2. Manually promote a healthy ASYNC replica to SYNC")
		return fmt.Errorf("no SYNC replica available - manual intervention required")
	}

	// Check if SYNC replica pod exists and is healthy
	syncPod, exists := clusterState.Pods[syncReplicaName]
	if !exists {
		log.Printf("🚨 SYNC replica pod %s no longer exists", syncReplicaName)
		return c.handleSyncReplicaFailure(ctx, clusterState, syncReplicaName)
	}

	if !c.isPodHealthyForMain(syncPod) {
		log.Printf("🚨 SYNC replica pod %s is unhealthy", syncReplicaName)
		return c.handleSyncReplicaFailure(ctx, clusterState, syncReplicaName)
	}

	// Check replica health from main's perspective
	if syncReplicaInfo != nil {
		if syncReplicaInfo.ParsedDataInfo != nil {
			if syncReplicaInfo.ParsedDataInfo.IsHealthy {
				log.Printf("✅ SYNC replica %s health check passed (behind: %d, status: %s)",
					syncReplicaName, syncReplicaInfo.ParsedDataInfo.Behind, syncReplicaInfo.ParsedDataInfo.Status)
			} else {
				log.Printf("⚠️  SYNC replica %s health issue: %s", syncReplicaName, syncReplicaInfo.ParsedDataInfo.ErrorReason)
			}
		} else {
			// No detailed info available - this indicates a parsing or data retrieval problem
			log.Printf("⚠️  SYNC replica %s health issue: no detailed info available", syncReplicaName)
		}
	}

	return nil
}

// handleSyncReplicaFailure responds to SYNC replica becoming unavailable with enhanced emergency procedures
func (c *MemgraphController) handleSyncReplicaFailure(ctx context.Context, clusterState *ClusterState, failedSyncReplica string) error {
	currentMain := clusterState.CurrentMain
	if currentMain == "" {
		return fmt.Errorf("no main available during SYNC replica failure")
	}

	log.Printf("🚨 HANDLING SYNC REPLICA FAILURE: %s", failedSyncReplica)
	log.Printf("⚠️  WARNING: Cluster in degraded state - writes may block until SYNC replica restored")

	mainPod := clusterState.Pods[currentMain]

	// Strategy 1: Try to restore the failed SYNC replica first (fastest recovery)
	log.Printf("Strategy 1: Attempting to restore failed SYNC replica %s", failedSyncReplica)
	if syncPod, exists := clusterState.Pods[failedSyncReplica]; exists && c.isPodHealthyForMain(syncPod) {
		log.Printf("SYNC replica %s appears to be healthy now, attempting to restore", failedSyncReplica)
		if err := c.configurePodAsSyncReplica(ctx, clusterState, failedSyncReplica); err == nil {
			log.Printf("✅ Successfully restored SYNC replica %s", failedSyncReplica)
			return nil
		} else {
			log.Printf("Failed to restore SYNC replica %s: %v", failedSyncReplica, err)
		}
	}

	// Strategy 2: Drop failed SYNC replica registration to unblock writes
	log.Printf("Strategy 2: Dropping failed SYNC replica registration to unblock writes")
	failedReplicaName := strings.ReplaceAll(failedSyncReplica, "-", "_")
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, mainPod.BoltAddress, failedReplicaName); err != nil {
		log.Printf("Warning: Failed to drop failed SYNC replica %s: %v", failedReplicaName, err)
	} else {
		log.Printf("Dropped failed SYNC replica registration: %s", failedReplicaName)
	}

	// No automatic ASYNC→SYNC promotion - require manual intervention
	log.Printf("🚨 CRITICAL: SYNC replica failure requires manual intervention")
	log.Printf("💡 Manual recovery options:")
	log.Printf("  1. Check pod health: kubectl get pods -n %s", c.config.Namespace)
	log.Printf("  2. Restart failed SYNC replica pod if it exists")
	log.Printf("  3. Manually promote a healthy ASYNC replica to SYNC if needed")
	log.Printf("  4. Monitor replication lag before promoting ASYNC replicas")
	return fmt.Errorf("SYNC replica failure - manual intervention required")
}

// configurePodAsSyncReplica configures a specific pod as SYNC replica
func (c *MemgraphController) configurePodAsSyncReplica(ctx context.Context, clusterState *ClusterState, podName string) error {
	currentMain := clusterState.CurrentMain
	if currentMain == "" {
		return fmt.Errorf("no main available for SYNC replica configuration")
	}

	mainPod := clusterState.Pods[currentMain]
	targetPod, exists := clusterState.Pods[podName]
	if !exists {
		return fmt.Errorf("target pod %s not found", podName)
	}

	log.Printf("Configuring %s as SYNC replica", podName)

	replicaName := targetPod.GetReplicaName()
	replicaAddress := targetPod.GetReplicationAddress()

	// Check if replica pod is ready for replication
	if replicaAddress == "" || !targetPod.IsReadyForReplication() {
		return fmt.Errorf("target pod %s not ready for replication (IP: %s, Ready: %v)",
			podName, replicaAddress, targetPod.IsReadyForReplication())
	}

	// Check if already configured as SYNC
	for _, replica := range mainPod.ReplicasInfo {
		if replica.Name == replicaName && replica.SyncMode == "sync" {
			targetPod.IsSyncReplica = true
			log.Printf("Pod %s already configured as SYNC replica", podName)
			return nil
		}
	}

	// Drop existing registration first (if any)
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, mainPod.BoltAddress, replicaName); err != nil {
		log.Printf("Warning: Could not drop existing replica %s: %v", replicaName, err)
	}

	// Register as SYNC replica
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, mainPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("failed to register SYNC replica %s: %w", replicaName, err)
	}

	// Update tracking
	targetPod.IsSyncReplica = true
	log.Printf("✅ Successfully configured %s as SYNC replica", podName)

	return nil
}

// SyncReplicaHealthStatus represents the health status of a SYNC replica
type SyncReplicaHealthStatus int

const (
	SyncReplicaHealthy SyncReplicaHealthStatus = iota
	SyncReplicaUnhealthy
	SyncReplicaFailed
)
