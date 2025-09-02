package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// isPodHealthyForMain determines if a pod is healthy enough to be main
func (c *MemgraphController) isPodHealthyForMain(node *MemgraphNode) bool {
	if node == nil {
		return false
	}

	// Pod must have a Bolt address for client connections
	if node.BoltAddress == "" {
		log.Printf("Pod %s not healthy for main: no bolt address", node.Name)
		return false
	}

	// Pod must have Memgraph role information
	if node.MemgraphRole == "" {
		log.Printf("Pod %s not healthy for main: no Memgraph role info", node.Name)
		return false
	}

	return true
}

// ConfigureReplication configures replication for the cluster
func (c *MemgraphController) ConfigureReplication(ctx context.Context, cluster *MemgraphCluster) error {
	if len(cluster.MemgraphNodes) == 0 {
		log.Println("No pods to configure replication for")
		return nil
	}

	log.Println("Starting replication configuration...")

	// Get the current main from controller's target main index
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main index: %w", err)
	}

	currentMain := c.config.GetPodName(targetMainIndex)
	if currentMain == "" {
		return fmt.Errorf("no main pod selected for replication configuration")
	}

	log.Printf("Configuring replication with main: %s (index %d)", currentMain, targetMainIndex)

	// Phase 1: Configure pod roles (MAIN/REPLICA)
	var configErrors []error

	for podName, node := range cluster.MemgraphNodes {
		if !node.NeedsReplicationConfiguration(currentMain) {
			log.Printf("Pod %s already in correct replication state", podName)
			continue
		}

		if node.ShouldBecomeMain(currentMain) {
			// Check if pod is healthy before attempting promotion
			if !c.isPodHealthyForMain(node) {
				log.Printf("Cannot promote unhealthy pod %s to MAIN (likely deleted or terminating)", podName)
				configErrors = append(configErrors, fmt.Errorf("promote %s to MAIN: pod is unhealthy", podName))
				continue
			}

			log.Printf("Promoting pod %s to MAIN role", podName)
			if err := node.SetToMainRole(ctx); err != nil {
				log.Printf("Failed to promote pod %s to MAIN: %v", podName, err)
				configErrors = append(configErrors, fmt.Errorf("promote %s to MAIN: %w", podName, err))
				continue
			}
			log.Printf("Successfully promoted pod %s to MAIN", podName)
		}

		if node.ShouldBecomeReplica(currentMain) {
			// Check if pod is healthy before attempting demotion
			if !c.isPodHealthyForMain(node) {
				log.Printf("Skipping demotion of unhealthy pod %s (likely deleted or terminating)", podName)
				continue
			}

			log.Printf("Demoting pod %s to REPLICA role", podName)
			if err := node.SetToReplicaRole(ctx); err != nil {
				log.Printf("Failed to demote pod %s to REPLICA: %v", podName, err)
				configErrors = append(configErrors, fmt.Errorf("demote %s to REPLICA: %w", podName, err))
				continue
			}
			log.Printf("Successfully demoted pod %s to REPLICA", podName)
		}
	}

	// Phase 2: Configure SYNC/ASYNC replication strategy with controller state authority
	if err := c.configureReplicationWithEnhancedSyncStrategy(ctx, cluster); err != nil {
		log.Printf("Failed to configure enhanced SYNC/ASYNC replication: %v", err)
		configErrors = append(configErrors, fmt.Errorf("enhanced SYNC/ASYNC replication configuration: %w", err))
	}

	// Phase 2.5: Monitor SYNC replica health and handle failures
	if err := c.detectSyncReplicaHealth(ctx, cluster); err != nil {
		log.Printf("SYNC replica health check failed: %v", err)
		configErrors = append(configErrors, fmt.Errorf("SYNC replica health monitoring: %w", err))
	}

	// Phase 3: Handle any existing replicas that should be removed
	// TEMPORARILY DISABLED: Conservative cleanup to prevent state corruption
	log.Printf("Skipping replica cleanup phase to preserve existing replication state")
	// if err := c.cleanupObsoleteReplicas(ctx, cluster); err != nil {
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

	log.Printf("Replication configuration completed successfully for %d pods", len(cluster.MemgraphNodes))
	return nil
}

// selectSyncReplica determines which replica should be configured as SYNC using controller state authority
func (c *MemgraphController) selectSyncReplica(cluster *MemgraphCluster, currentMain string) string {
	log.Printf("Selecting SYNC replica using deterministic controller strategy...")

	// Strategy: Use controller's two-pod main/SYNC strategy
	// If pod-0 is main, pod-1 is SYNC replica (and vice versa)
	currentMainIndex := c.config.ExtractPodIndex(currentMain)

	if currentMainIndex == 0 {
		// Main is pod-0, SYNC replica should be pod-1
		syncReplicaName := c.config.GetPodName(1)
		if _, exists := cluster.MemgraphNodes[syncReplicaName]; exists {
			log.Printf("Deterministic SYNC selection: %s (main=pod-0)", syncReplicaName)
			return syncReplicaName
		}
	} else if currentMainIndex == 1 {
		// Main is pod-1, SYNC replica should be pod-0
		syncReplicaName := c.config.GetPodName(0)
		if _, exists := cluster.MemgraphNodes[syncReplicaName]; exists {
			log.Printf("Deterministic SYNC selection: %s (main=pod-1)", syncReplicaName)
			return syncReplicaName
		}
	}

	// Fallback: main is not pod-0 or pod-1, or target SYNC replica doesn't exist
	// Use alphabetical selection as fallback
	log.Printf("Fallback SYNC selection: main index=%d not in eligible range", currentMainIndex)
	return c.selectSyncReplicaFallback(cluster, currentMain)
}

// selectSyncReplicaFallback uses alphabetical selection as fallback
func (c *MemgraphController) selectSyncReplicaFallback(cluster *MemgraphCluster, currentMain string) string {
	var eligibleReplicas []string

	// Collect all non-main pods as potential SYNC replicas
	for podName := range cluster.MemgraphNodes {
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

// configureReplicationWithEnhancedSyncStrategy configures replication using enhanced SYNC/ASYNC strategy with controller authority
func (c *MemgraphController) configureReplicationWithEnhancedSyncStrategy(ctx context.Context, cluster *MemgraphCluster) error {
	log.Printf("Configuring enhanced SYNC/ASYNC replication with controller state authority...")

	// Get the current main from controller's target main index
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main index: %w", err)
	}

	currentMain := c.config.GetPodName(targetMainIndex)
	if currentMain == "" {
		return fmt.Errorf("no main pod selected for enhanced SYNC replication configuration")
	}

	_, exists := cluster.MemgraphNodes[currentMain]
	if !exists {
		return fmt.Errorf("main pod %s not found in cluster state", currentMain)
	}

	// Use controller state authority to determine SYNC replica
	targetSyncReplica := c.selectSyncReplica(cluster, currentMain)

	log.Printf("Enhanced SYNC strategy: target=%s, main=%s",
		targetSyncReplica, currentMain)

	var configErrors []error

	// Phase 1: Ensure SYNC replica is configured correctly
	if err := c.ensureCorrectSyncReplica(ctx, cluster, targetSyncReplica); err != nil {
		configErrors = append(configErrors, fmt.Errorf("SYNC replica configuration: %w", err))
	}

	// Phase 2: Configure remaining pods as ASYNC replicas
	if err := c.configureAsyncReplicas(ctx, cluster, targetSyncReplica); err != nil {
		configErrors = append(configErrors, fmt.Errorf("ASYNC replica configuration: %w", err))
	}

	// Refresh replica information from main to get current state after configuration
	mainPod := cluster.MemgraphNodes[currentMain]
	if replicasResp, err := mainPod.QueryReplicas(ctx); err != nil {
		log.Printf("Warning: Failed to refresh replicas info from main %s: %v", currentMain, err)
	} else {
		mainPod.ReplicasInfo = replicasResp.Replicas
		log.Printf("Refreshed replica information from main - found %d replicas", len(mainPod.ReplicasInfo))
	}

	// Phase 3: Verify final SYNC replica configuration
	if err := c.verifySyncReplicaConfiguration(ctx, cluster); err != nil {
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
func (c *MemgraphController) ensureCorrectSyncReplica(ctx context.Context, cluster *MemgraphCluster, targetSyncReplica string) error {
	if targetSyncReplica == "" {
		return fmt.Errorf("no target SYNC replica specified")
	}

	// Simply configure the target pod as SYNC replica (idempotent operation)
	if err := c.configurePodAsSyncReplica(ctx, cluster, targetSyncReplica); err != nil {
		return fmt.Errorf("failed to configure SYNC replica %s: %w", targetSyncReplica, err)
	}

	log.Printf("✅ Configured %s as SYNC replica", targetSyncReplica)
	return nil
}

// configureAsyncReplicas configures all non-main, non-SYNC pods as ASYNC replicas
func (c *MemgraphController) configureAsyncReplicas(ctx context.Context, cluster *MemgraphCluster, syncReplicaPod string) error {
	// Get the current main from controller's target main index
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main index: %w", err)
	}

	currentMain := c.config.GetPodName(targetMainIndex)
	mainPod := cluster.MemgraphNodes[currentMain]
	var configErrors []error

	for podName, node := range cluster.MemgraphNodes {
		// Skip main and SYNC replica
		if podName == currentMain || podName == syncReplicaPod {
			continue
		}

		// Configure as ASYNC replica
		if err := c.configurePodAsAsyncReplica(ctx, mainPod, node); err != nil {
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
func (c *MemgraphController) configurePodAsAsyncReplica(ctx context.Context, mainPod, replicaPod *MemgraphNode) error {
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
	if err := mainPod.DropReplica(ctx, replicaName); err != nil {
		log.Printf("Warning: Could not drop existing replica %s: %v", replicaName, err)
	}

	// Register as ASYNC replica
	if err := mainPod.RegisterReplicaWithMode(ctx, replicaName, replicaAddress, "ASYNC"); err != nil {
		return fmt.Errorf("failed to register ASYNC replica %s: %w", replicaName, err)
	}

	// Update tracking
	replicaPod.IsSyncReplica = false
	return nil
}

// verifyPodSyncConfiguration verifies that a pod is correctly configured as SYNC replica

// verifySyncReplicaConfiguration verifies the final SYNC replica configuration
func (c *MemgraphController) verifySyncReplicaConfiguration(ctx context.Context, cluster *MemgraphCluster) error {
	// Count SYNC replicas
	syncReplicaCount := 0
	var syncReplicaName string

	for podName, node := range cluster.MemgraphNodes {
		if node.IsSyncReplica {
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
		// Verify SYNC replica health from main's perspective
		targetMainIndex, err := c.GetTargetMainIndex(ctx)
		if err != nil {
			return fmt.Errorf("failed to get target main index: %w", err)
		}

		currentMain := c.config.GetPodName(targetMainIndex)
		if currentMain == "" {
			log.Printf("⚠️  WARNING: Cannot verify SYNC replica health - no main available")
			return fmt.Errorf("no main available to verify SYNC replica")
		}

		mainPod, exists := cluster.MemgraphNodes[currentMain]
		if !exists {
			return fmt.Errorf("main pod %s not found in cluster state", currentMain)
		}

		// Find the SYNC replica info from main's perspective
		syncReplicaPodName := strings.ReplaceAll(syncReplicaName, "-", "_")
		var syncReplicaInfo *ReplicaInfo
		for _, replica := range mainPod.ReplicasInfo {
			if replica.Name == syncReplicaPodName && replica.SyncMode == "sync" {
				syncReplicaInfo = &replica
				break
			}
		}

		if syncReplicaInfo == nil {
			log.Printf("❌ SYNC replica %s not found in main's replica list", syncReplicaName)
			return fmt.Errorf("SYNC replica %s not registered with main", syncReplicaName)
		}

		// Verify data_info health status
		if syncReplicaInfo.ParsedDataInfo != nil {
			if !syncReplicaInfo.ParsedDataInfo.IsHealthy {
				log.Printf("⚠️  SYNC replica %s is unhealthy: %s", syncReplicaName, syncReplicaInfo.ParsedDataInfo.ErrorReason)
				return fmt.Errorf("SYNC replica %s unhealthy: %s", syncReplicaName, syncReplicaInfo.ParsedDataInfo.ErrorReason)
			}

			// Check replication lag
			if syncReplicaInfo.ParsedDataInfo.Behind > 0 {
				log.Printf("⚠️  SYNC replica %s has replication lag: %d transactions behind", syncReplicaName, syncReplicaInfo.ParsedDataInfo.Behind)
				// For SYNC replicas, any lag is concerning but not necessarily an error during initial setup
				if syncReplicaInfo.ParsedDataInfo.Behind > 10 {
					return fmt.Errorf("SYNC replica %s has excessive lag: %d transactions behind", syncReplicaName, syncReplicaInfo.ParsedDataInfo.Behind)
				}
			}

			// Verify status is "ready"
			if syncReplicaInfo.ParsedDataInfo.Status != "ready" {
				log.Printf("⚠️  SYNC replica %s status is '%s', expected 'ready'", syncReplicaName, syncReplicaInfo.ParsedDataInfo.Status)
				return fmt.Errorf("SYNC replica %s has non-ready status: %s", syncReplicaName, syncReplicaInfo.ParsedDataInfo.Status)
			}

			log.Printf("✅ SYNC replica verification passed: %s (status: %s, lag: %d)",
				syncReplicaName, syncReplicaInfo.ParsedDataInfo.Status, syncReplicaInfo.ParsedDataInfo.Behind)
		} else {
			// No parsed data info available, check raw data_info
			if syncReplicaInfo.DataInfo == "" || syncReplicaInfo.DataInfo == "Null" {
				log.Printf("⚠️  WARNING: SYNC replica %s has no data_info - may still be initializing", syncReplicaName)
				// Don't fail immediately as replica might be initializing
			} else {
				log.Printf("✅ SYNC replica verification passed: %s (raw data_info: %s)", syncReplicaName, syncReplicaInfo.DataInfo)
			}
		}

		return nil

	default:
		log.Printf("❌ ERROR: Multiple SYNC replicas detected (%d) - this should not happen", syncReplicaCount)
		return fmt.Errorf("multiple SYNC replicas detected: %d", syncReplicaCount)
	}
}

// detectSyncReplicaHealth monitors SYNC replica health and handles failures
func (c *MemgraphController) detectSyncReplicaHealth(ctx context.Context, cluster *MemgraphCluster) error {
	// Get the current main from controller's target main index
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main index: %w", err)
	}

	currentMain := c.config.GetPodName(targetMainIndex)
	if currentMain == "" {
		return fmt.Errorf("no main available for SYNC replica health monitoring")
	}

	mainPod, exists := cluster.MemgraphNodes[currentMain]
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
	syncPod, exists := cluster.MemgraphNodes[syncReplicaName]
	if !exists {
		log.Printf("🚨 SYNC replica pod %s no longer exists", syncReplicaName)
		return c.handleSyncReplicaFailure(ctx, cluster, syncReplicaName)
	}

	if !c.isPodHealthyForMain(syncPod) {
		log.Printf("🚨 SYNC replica pod %s is unhealthy", syncReplicaName)
		return c.handleSyncReplicaFailure(ctx, cluster, syncReplicaName)
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
func (c *MemgraphController) handleSyncReplicaFailure(ctx context.Context, cluster *MemgraphCluster, failedSyncReplica string) error {
	// Get the current main from controller's target main index
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main index: %w", err)
	}

	currentMain := c.config.GetPodName(targetMainIndex)
	if currentMain == "" {
		return fmt.Errorf("no main available during SYNC replica failure")
	}

	log.Printf("🚨 HANDLING SYNC REPLICA FAILURE: %s", failedSyncReplica)
	log.Printf("⚠️  WARNING: Cluster in degraded state - writes may block until SYNC replica restored")

	mainPod := cluster.MemgraphNodes[currentMain]

	// Strategy 1: Try to restore the failed SYNC replica first (fastest recovery)
	log.Printf("Strategy 1: Attempting to restore failed SYNC replica %s", failedSyncReplica)
	if syncPod, exists := cluster.MemgraphNodes[failedSyncReplica]; exists && c.isPodHealthyForMain(syncPod) {
		log.Printf("SYNC replica %s appears to be healthy now, attempting to restore", failedSyncReplica)
		if err := c.configurePodAsSyncReplica(ctx, cluster, failedSyncReplica); err == nil {
			log.Printf("✅ Successfully restored SYNC replica %s", failedSyncReplica)
			return nil
		} else {
			log.Printf("Failed to restore SYNC replica %s: %v", failedSyncReplica, err)
		}
	}

	// Strategy 2: Drop failed SYNC replica registration to unblock writes
	log.Printf("Strategy 2: Dropping failed SYNC replica registration to unblock writes")
	failedReplicaName := strings.ReplaceAll(failedSyncReplica, "-", "_")
	if err := mainPod.DropReplica(ctx, failedReplicaName); err != nil {
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
func (c *MemgraphController) configurePodAsSyncReplica(ctx context.Context, cluster *MemgraphCluster, podName string) error {
	// Get the current main from controller's target main index
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		return fmt.Errorf("failed to get target main index: %w", err)
	}

	currentMain := c.config.GetPodName(targetMainIndex)
	if currentMain == "" {
		return fmt.Errorf("no main available for SYNC replica configuration")
	}

	mainPod := cluster.MemgraphNodes[currentMain]
	targetPod, exists := cluster.MemgraphNodes[podName]
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
	if err := mainPod.DropReplica(ctx, replicaName); err != nil {
		log.Printf("Warning: Could not drop existing replica %s: %v", replicaName, err)
	}

	// Register as SYNC replica
	if err := mainPod.RegisterReplicaWithMode(ctx, replicaName, replicaAddress, "SYNC"); err != nil {
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
