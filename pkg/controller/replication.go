package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
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
	replicaAddress := replicaPod.GetReplicationAddressByIP()

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
	replicaAddress := syncPod.GetReplicationAddressByIP()

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

// configureReplicationWithSyncStrategy configures replication using SYNC/ASYNC strategy (legacy method)
func (c *MemgraphController) configureReplicationWithSyncStrategy(ctx context.Context, clusterState *ClusterState) error {
	currentMain := clusterState.CurrentMain
	if currentMain == "" {
		return fmt.Errorf("no main pod selected for SYNC replication configuration")
	}

	mainPod, exists := clusterState.Pods[currentMain]
	if !exists {
		return fmt.Errorf("main pod %s not found in cluster state", currentMain)
	}

	log.Printf("Configuring SYNC/ASYNC replication with main: %s", currentMain)

	// Identify or select SYNC replica
	currentSyncReplica := c.identifySyncReplica(clusterState, currentMain)
	targetSyncReplica := c.selectSyncReplica(clusterState, currentMain)

	if targetSyncReplica == "" {
		log.Println("No replicas available for SYNC configuration")
		return nil
	}

	log.Printf("Target SYNC replica: %s (current: %s)", targetSyncReplica, currentSyncReplica)

	var configErrors []error

	// Phase 1: Configure SYNC replica (highest priority - must succeed)
	if targetSyncReplica != currentSyncReplica {
		log.Printf("Configuring %s as SYNC replica", targetSyncReplica)

		targetPod, exists := clusterState.Pods[targetSyncReplica]
		if !exists {
			return fmt.Errorf("target SYNC replica pod %s not found", targetSyncReplica)
		}

		replicaName := targetPod.GetReplicaName()
		replicaAddress := targetPod.GetReplicationAddressByIP()

		// Check if replica pod is ready for replication
		if replicaAddress == "" || !targetPod.IsReadyForReplication() {
			return fmt.Errorf("target SYNC replica pod %s not ready for replication (IP: %s, Ready: %v)",
				targetPod.Name, replicaAddress, targetPod.IsReadyForReplication())
		}

		// Check if already configured as SYNC
		isAlreadySyncReplica := false
		currentMode := ""
		for _, replica := range mainPod.ReplicasInfo {
			if replica.Name == replicaName {
				currentMode = replica.SyncMode
				if currentMode == "sync" {
					isAlreadySyncReplica = true
				}
				break
			}
		}

		if isAlreadySyncReplica {
			log.Printf("SYNC replica %s already correctly configured, skipping", replicaName)
			targetPod.IsSyncReplica = true
		} else {
			// Only drop if changing mode (ASYNC -> SYNC) or not registered
			if currentMode != "" && currentMode != "sync" {
				log.Printf("Changing replica %s from %s to SYNC mode", replicaName, currentMode)
				if err := c.memgraphClient.DropReplicaWithRetry(ctx, mainPod.BoltAddress, replicaName); err != nil {
					log.Printf("Warning: Could not drop existing replica %s: %v", replicaName, err)
				}
			}

			// Register as SYNC replica (CRITICAL - must succeed)
			if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, mainPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
				configErrors = append(configErrors, fmt.Errorf("CRITICAL: failed to register SYNC replica %s: %w", replicaName, err))
				return fmt.Errorf("CRITICAL: Cannot guarantee data consistency without SYNC replica: %w", err)
			}

			log.Printf("Successfully registered SYNC replica: %s", replicaName)

			// Update pod tracking
			targetPod.IsSyncReplica = true
		}

		// Clear old SYNC replica flag if different
		if currentSyncReplica != "" && currentSyncReplica != targetSyncReplica {
			if oldSyncPod, exists := clusterState.Pods[currentSyncReplica]; exists {
				oldSyncPod.IsSyncReplica = false
			}
		}
	}

	// Phase 2: Configure ASYNC replicas (failures are warnings, not critical)
	for podName, podInfo := range clusterState.Pods {
		// Skip main pod and SYNC replica
		if podName == currentMain || podName == targetSyncReplica {
			continue
		}

		// Ensure pod role is replica
		if podInfo.MemgraphRole != "replica" {
			continue
		}

		// Configure as ASYNC replica
		if err := c.configurePodAsAsyncReplica(ctx, mainPod, podInfo); err != nil {
			log.Printf("Warning: Failed to configure ASYNC replica %s: %v", podName, err)
			configErrors = append(configErrors, fmt.Errorf("ASYNC replica %s: %w", podName, err))
		} else {
			log.Printf("Successfully configured ASYNC replica: %s", podName)
		}
	}

	// Check for critical errors
	criticalErrors := 0
	for _, err := range configErrors {
		if strings.Contains(err.Error(), "CRITICAL") {
			criticalErrors++
		}
	}

	if criticalErrors > 0 {
		return fmt.Errorf("CRITICAL: SYNC replication configuration failed - cannot guarantee data safety")
	}

	if len(configErrors) > 0 {
		log.Printf("Replication configuration completed with %d warnings (ASYNC replicas)", len(configErrors)-criticalErrors)
		return fmt.Errorf("replication configuration had %d warnings (see logs)", len(configErrors)-criticalErrors)
	}

	log.Printf("SYNC/ASYNC replication configuration completed successfully")
	return nil
}

// promoteAsyncToSync promotes an ASYNC replica to SYNC mode
func (c *MemgraphController) promoteAsyncToSync(ctx context.Context, clusterState *ClusterState, targetReplicaPod string) error {
	currentMain := clusterState.CurrentMain
	if currentMain == "" {
		return fmt.Errorf("no main pod available for ASYNC→SYNC promotion")
	}

	mainPod, exists := clusterState.Pods[currentMain]
	if !exists {
		return fmt.Errorf("main pod %s not found", currentMain)
	}

	targetPod, exists := clusterState.Pods[targetReplicaPod]
	if !exists {
		return fmt.Errorf("target replica pod %s not found", targetReplicaPod)
	}

	log.Printf("🔄 ASYNC→SYNC PROMOTION: %s", targetReplicaPod)

	// Pre-promotion validation
	if err := c.validateAsyncToSyncPromotion(ctx, clusterState, targetReplicaPod); err != nil {
		return fmt.Errorf("ASYNC→SYNC promotion validation failed: %w", err)
	}

	replicaName := targetPod.GetReplicaName()
	replicaAddress := targetPod.GetReplicationAddressByIP()

	// Check if replica pod is ready for replication
	if replicaAddress == "" || !targetPod.IsReadyForReplication() {
		return fmt.Errorf("target replica pod %s not ready for replication (IP: %s, Ready: %v)",
			targetPod.Name, replicaAddress, targetPod.IsReadyForReplication())
	}

	// Step 1: Check if replica is caught up
	log.Printf("Checking if %s is caught up with main...", replicaName)
	if err := c.verifyReplicaSyncStatus(ctx, mainPod, replicaName); err != nil {
		log.Printf("⚠️  WARNING: Replica may not be fully caught up: %v", err)
		log.Printf("⚠️  Proceeding with promotion - verify data consistency manually")
	}

	// Step 2: Drop existing ASYNC registration
	log.Printf("Dropping existing ASYNC replica registration: %s", replicaName)
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, mainPod.BoltAddress, replicaName); err != nil {
		return fmt.Errorf("failed to drop ASYNC replica %s: %w", replicaName, err)
	}

	// Step 3: Re-register as SYNC replica
	log.Printf("Re-registering %s as SYNC replica", replicaName)
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, mainPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("failed to register SYNC replica %s: %w", replicaName, err)
	}

	// Step 4: Update tracking and clear old SYNC replica
	c.updateSyncReplicaTracking(clusterState, targetReplicaPod)

	// Step 5: Verify promotion success
	if err := c.verifyAsyncToSyncPromotion(ctx, clusterState, targetReplicaPod); err != nil {
		log.Printf("❌ SYNC promotion verification failed: %v", err)
		return fmt.Errorf("ASYNC→SYNC promotion verification failed: %w", err)
	}

	log.Printf("✅ Successfully promoted %s to SYNC replica", targetReplicaPod)
	log.Printf("📊 PROMOTION EVENT: %s promoted from ASYNC to SYNC", targetReplicaPod)

	return nil
}

// validateAsyncToSyncPromotion validates preconditions for ASYNC→SYNC promotion
func (c *MemgraphController) validateAsyncToSyncPromotion(ctx context.Context, clusterState *ClusterState, targetReplica string) error {
	// Check 1: Target replica must exist and be healthy
	targetPod, exists := clusterState.Pods[targetReplica]
	if !exists {
		return fmt.Errorf("target replica %s not found", targetReplica)
	}

	if !c.isPodHealthyForMain(targetPod) {
		return fmt.Errorf("target replica %s is not healthy", targetReplica)
	}

	// Check 2: Target replica must have replica role
	if targetPod.MemgraphRole != "replica" {
		return fmt.Errorf("target pod %s has role %s, expected replica", targetReplica, targetPod.MemgraphRole)
	}

	// Check 3: Target replica must not already be SYNC
	if targetPod.IsSyncReplica {
		return fmt.Errorf("target replica %s is already a SYNC replica", targetReplica)
	}

	// Check 4: Main must be available
	mainPod, exists := clusterState.Pods[clusterState.CurrentMain]
	if !exists || !c.isPodHealthyForMain(mainPod) {
		return fmt.Errorf("main is not available for ASYNC→SYNC promotion")
	}

	log.Printf("✅ ASYNC→SYNC promotion validation passed for %s", targetReplica)
	return nil
}

// verifyReplicaSyncStatus checks if replica is caught up with main
func (c *MemgraphController) verifyReplicaSyncStatus(ctx context.Context, mainPod *PodInfo, replicaName string) error {
	// Check if replica exists in main's replica list
	var replicaInfo *ReplicaInfo
	for _, replica := range mainPod.ReplicasInfo {
		if replica.Name == replicaName {
			replicaInfo = &replica
			break
		}
	}

	if replicaInfo == nil {
		return fmt.Errorf("replica %s not registered with main", replicaName)
	}

	// Check if replica is lagging significantly
	if replicaInfo.SystemTimestamp == 0 {
		log.Printf("⚠️  WARNING: Replica %s has zero system timestamp - may not be caught up", replicaName)
		return fmt.Errorf("replica %s has zero system timestamp", replicaName)
	}

	log.Printf("Replica %s sync status: timestamp=%d, mode=%s",
		replicaName, replicaInfo.SystemTimestamp, replicaInfo.SyncMode)

	return nil
}

// updateSyncReplicaTracking updates SYNC replica tracking after promotion
func (c *MemgraphController) updateSyncReplicaTracking(clusterState *ClusterState, newSyncReplica string) {
	// Clear old SYNC replica flags
	for _, podInfo := range clusterState.Pods {
		podInfo.IsSyncReplica = false
	}

	// Set new SYNC replica flag
	if newSyncPod, exists := clusterState.Pods[newSyncReplica]; exists {
		newSyncPod.IsSyncReplica = true
		log.Printf("Updated SYNC replica tracking: %s is now the SYNC replica", newSyncReplica)
	}
}

// verifyAsyncToSyncPromotion verifies that ASYNC→SYNC promotion succeeded
func (c *MemgraphController) verifyAsyncToSyncPromotion(ctx context.Context, clusterState *ClusterState, promotedReplica string) error {
	// Wait a moment for Memgraph to update replica information
	time.Sleep(100 * time.Millisecond)

	// Re-query main's replica information to get updated status
	currentMain := clusterState.CurrentMain
	mainPod := clusterState.Pods[currentMain]

	// Refresh main pod's replica information
	if refreshedPod, err := c.RefreshPodInfo(ctx, mainPod.Name); err != nil {
		log.Printf("Warning: Could not refresh main replica info: %v", err)
	} else {
		*mainPod = *refreshedPod
	}

	promotedPod := clusterState.Pods[promotedReplica]
	replicaName := promotedPod.GetReplicaName()

	// Check if promoted replica is now registered as SYNC
	for _, replica := range mainPod.ReplicasInfo {
		if replica.Name == replicaName {
			if replica.SyncMode == "sync" {
				log.Printf("✅ Promotion verification passed: %s is now SYNC", promotedReplica)
				return nil
			}
			return fmt.Errorf("replica %s still has sync_mode=%s after promotion", promotedReplica, replica.SyncMode)
		}
	}

	return fmt.Errorf("replica %s not found in main's replica list after promotion", promotedReplica)
}

// emergencyAsyncToSyncPromotion handles emergency SYNC replica promotion when SYNC replica fails
func (c *MemgraphController) emergencyAsyncToSyncPromotion(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("🚨 EMERGENCY: Performing ASYNC→SYNC promotion due to SYNC replica failure")

	// Find best candidate for ASYNC→SYNC promotion
	candidateReplica := c.selectBestAsyncReplicaForSyncPromotion(clusterState)
	if candidateReplica == "" {
		return fmt.Errorf("no suitable ASYNC replica available for emergency SYNC promotion")
	}

	log.Printf("🚨 EMERGENCY PROMOTION: Promoting %s from ASYNC to SYNC", candidateReplica)

	// Perform emergency promotion
	if err := c.promoteAsyncToSync(ctx, clusterState, candidateReplica); err != nil {
		return fmt.Errorf("emergency ASYNC→SYNC promotion failed: %w", err)
	}

	log.Printf("✅ EMERGENCY: Successfully promoted %s to SYNC replica", candidateReplica)
	log.Printf("📊 EMERGENCY EVENT: ASYNC→SYNC promotion completed due to SYNC replica failure")

	return nil
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
		return c.emergencyAsyncToSyncPromotion(ctx, clusterState)
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
		if syncReplicaInfo.SystemTimestamp == 0 {
			log.Printf("⚠️  SYNC replica %s has zero timestamp - potential health issue", syncReplicaName)
		}

		log.Printf("✅ SYNC replica %s health check passed (timestamp: %d)",
			syncReplicaName, syncReplicaInfo.SystemTimestamp)
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

	// Strategy 3: Emergency ASYNC→SYNC promotion
	log.Printf("Strategy 3: Emergency ASYNC→SYNC promotion")
	if err := c.emergencyAsyncToSyncPromotion(ctx, clusterState); err != nil {
		log.Printf("❌ Emergency ASYNC→SYNC promotion failed: %v", err)
		log.Printf("🚨 CRITICAL: Cluster remains in degraded state without SYNC replica")
		log.Printf("💡 Manual intervention required:")
		log.Printf("  1. Check pod health: kubectl get pods")
		log.Printf("  2. Restart failed SYNC replica pod if needed")
		log.Printf("  3. Consider manual ASYNC→SYNC promotion")
		return fmt.Errorf("SYNC replica failure recovery failed - manual intervention required")
	}

	log.Printf("✅ SYNC replica failure handled successfully")
	return nil
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
	replicaAddress := targetPod.GetReplicationAddressByIP()

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

// shouldPerformEmergencyPromotion determines if automatic emergency promotion should be performed
func (c *MemgraphController) shouldPerformEmergencyPromotion(clusterState *ClusterState) bool {
	// Conservative approach: only perform automatic promotion if:
	// 1. We have a suitable candidate
	// 2. The candidate is the expected SYNC replica pod (pod-0 or pod-1)
	// 3. The cluster is in a healthy state otherwise

	bestCandidate := c.selectBestAsyncReplicaForSyncPromotion(clusterState)
	if bestCandidate == "" {
		return false
	}

	// Only auto-promote if candidate is pod-0 or pod-1 (expected SYNC replica range)
	candidateIndex := c.config.ExtractPodIndex(bestCandidate)
	if candidateIndex < 0 || candidateIndex > 1 {
		log.Printf("Emergency candidate %s is not in expected SYNC range (index %d) - requiring manual intervention",
			bestCandidate, candidateIndex)
		return false
	}

	// Check cluster health
	healthSummary := clusterState.GetClusterHealthSummary()
	healthyPods := healthSummary["healthy_pods"].(int)
	totalPods := healthSummary["total_pods"].(int)

	if healthyPods < totalPods/2 {
		log.Printf("Cluster health insufficient for automatic promotion: %d/%d pods healthy", healthyPods, totalPods)
		return false
	}

	log.Printf("✅ Automatic emergency promotion criteria met for %s", bestCandidate)
	return true
}

// provideManaualRecoveryInstructions provides detailed manual recovery instructions
func (c *MemgraphController) provideManaualRecoveryInstructions(clusterState *ClusterState, failedSyncReplica, bestCandidate string) error {
	log.Printf("🔧 Manual SYNC replica recovery instructions:")
	log.Printf("")
	log.Printf("Option 1: Restart failed SYNC replica (preferred)")
	log.Printf("  kubectl delete pod %s -n %s", failedSyncReplica, c.config.Namespace)
	log.Printf("  # Wait for pod to restart and reconnect")
	log.Printf("")
	log.Printf("Option 2: Emergency ASYNC→SYNC promotion")
	if bestCandidate != "" {
		replicaName := strings.ReplaceAll(bestCandidate, "-", "_")
		log.Printf("  # Drop failed SYNC replica:")
		log.Printf("  kubectl exec %s -n %s -- mgconsole -e \"DROP REPLICA %s;\"",
			clusterState.CurrentMain, c.config.Namespace, strings.ReplaceAll(failedSyncReplica, "-", "_"))
		log.Printf("  # Promote ASYNC replica to SYNC:")
		log.Printf("  kubectl exec %s -n %s -- mgconsole -e \"DROP REPLICA %s; REGISTER REPLICA %s SYNC TO \\\"%s.%s:10000\\\";\"",
			clusterState.CurrentMain, c.config.Namespace, replicaName, replicaName, bestCandidate, c.config.ServiceName)
	}
	log.Printf("")
	log.Printf("Option 3: Drop SYNC requirement (data loss risk)")
	log.Printf("  kubectl exec %s -n %s -- mgconsole -e \"DROP REPLICA %s;\"",
		clusterState.CurrentMain, c.config.Namespace, strings.ReplaceAll(failedSyncReplica, "-", "_"))
	log.Printf("  # WARNING: Writes will resume but data consistency not guaranteed")
	log.Printf("")
	log.Printf("⚠️  After manual intervention, restart the controller to resync state")

	return fmt.Errorf("SYNC replica failure requires manual intervention - see recovery instructions above")
}

// checkSyncReplicaHealth performs comprehensive health check on SYNC replica
func (c *MemgraphController) checkSyncReplicaHealth(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) SyncReplicaHealthStatus {
	syncPod := clusterState.Pods[syncReplicaPod]

	// Check 1: Pod must have IP address
	if syncPod.BoltAddress == "" {
		log.Printf("SYNC replica health check failed: no bolt address")
		return SyncReplicaFailed
	}

	// Check 2: Pod must have Memgraph role information
	if syncPod.MemgraphRole == "" {
		log.Printf("SYNC replica health check failed: no Memgraph role info")
		return SyncReplicaUnhealthy
	}

	// Check 3: Pod must have replica role
	if syncPod.MemgraphRole != "replica" {
		log.Printf("SYNC replica health check failed: role is %s, expected replica", syncPod.MemgraphRole)
		return SyncReplicaFailed
	}

	// Check 4: Test actual connectivity
	if err := c.memgraphClient.TestConnectionWithRetry(ctx, syncPod.BoltAddress); err != nil {
		log.Printf("SYNC replica health check failed: connectivity error: %v", err)
		return SyncReplicaFailed
	}

	// Check 5: Verify it's actually registered as SYNC with main
	mainPod := clusterState.Pods[clusterState.CurrentMain]
	if mainPod != nil {
		isRegisteredAsSync := false
		for _, replica := range mainPod.ReplicasInfo {
			if replica.Name == syncPod.GetReplicaName() && replica.SyncMode == "sync" {
				isRegisteredAsSync = true
				break
			}
		}

		if !isRegisteredAsSync {
			log.Printf("SYNC replica health check failed: not registered as SYNC with main")
			return SyncReplicaUnhealthy
		}
	}

	return SyncReplicaHealthy
}

// ensureSyncReplicaExists ensures a SYNC replica is configured when none exists
func (c *MemgraphController) ensureSyncReplicaExists(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Ensuring SYNC replica exists...")

	currentMain := clusterState.CurrentMain
	targetSyncReplica := c.selectSyncReplica(clusterState, currentMain)

	if targetSyncReplica == "" {
		log.Printf("No suitable pods available for SYNC replica configuration")
		return nil
	}

	log.Printf("Configuring missing SYNC replica: %s", targetSyncReplica)

	// Configure the selected pod as SYNC replica
	return c.configurePodAsSyncReplica(ctx, clusterState, targetSyncReplica)
}

// recoverSyncReplica attempts to recover an unhealthy SYNC replica
func (c *MemgraphController) recoverSyncReplica(ctx context.Context, clusterState *ClusterState, syncReplicaPod string) error {
	log.Printf("Attempting SYNC replica recovery for %s", syncReplicaPod)

	syncPod := clusterState.Pods[syncReplicaPod]
	mainPod := clusterState.Pods[clusterState.CurrentMain]

	if mainPod == nil {
		return fmt.Errorf("no main pod available for SYNC replica recovery")
	}

	// Attempt to re-register as SYNC replica
	replicaName := syncPod.GetReplicaName()
	replicaAddress := syncPod.GetReplicationAddressByIP()

	// Check if replica pod is ready for replication
	if replicaAddress == "" || !syncPod.IsReadyForReplication() {
		return fmt.Errorf("SYNC replica pod %s not ready for replication (IP: %s, Ready: %v)",
			syncPod.Name, replicaAddress, syncPod.IsReadyForReplication())
	}

	log.Printf("Re-registering %s as SYNC replica", replicaName)

	// Drop existing registration first
	if err := c.memgraphClient.DropReplicaWithRetry(ctx, mainPod.BoltAddress, replicaName); err != nil {
		log.Printf("Warning: Could not drop existing replica registration: %v", err)
	}

	// Re-register as SYNC
	if err := c.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, mainPod.BoltAddress, replicaName, replicaAddress, "SYNC"); err != nil {
		return fmt.Errorf("failed to re-register SYNC replica: %w", err)
	}

	log.Printf("Successfully recovered SYNC replica: %s", syncReplicaPod)
	return nil
}
