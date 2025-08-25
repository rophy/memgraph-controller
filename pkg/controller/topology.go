package controller

import (
	"context"
	"fmt"
	"log"
	"time"
)

// enhancedMainSelection applies enhanced main selection logic with SYNC replica priority
func (c *MemgraphController) enhancedMainSelection(clusterState *ClusterState) {
	log.Printf("Applying enhanced main selection logic...")

	// Priority-based main selection strategy:
	// 1. Controller's target main (if available and healthy)
	// 2. Existing MAIN node (avoid unnecessary failover)
	// 3. SYNC replica (guaranteed data consistency)
	// 4. Manual intervention required (no safe automatic promotion)

	targetMainName := c.config.GetPodName(clusterState.TargetMainIndex)
	var targetMain *PodInfo
	var existingMain *PodInfo
	var syncReplica *PodInfo

	// Analyze available pods
	for podName, podInfo := range clusterState.Pods {
		// Priority 1: Controller's target main
		if podName == targetMainName {
			targetMain = podInfo
		}

		// Priority 2: Existing MAIN node
		if podInfo.MemgraphRole == "main" {
			existingMain = podInfo
		}

		// Priority 3: SYNC replica
		if podInfo.IsSyncReplica {
			syncReplica = podInfo
		}
	}

	// Enhanced main selection decision tree
	var selectedMain *PodInfo
	var selectionReason string

	// Priority 1: Controller's target main (if healthy)
	if targetMain != nil && c.isPodHealthyForMain(targetMain) {
		selectedMain = targetMain
		selectionReason = fmt.Sprintf("controller target main (index %d)", clusterState.TargetMainIndex)
		log.Printf("✅ Using controller's target main: %s", targetMainName)

		// Priority 2: Existing MAIN node (avoid unnecessary failover)
	} else if existingMain != nil && c.isPodHealthyForMain(existingMain) {
		selectedMain = existingMain
		selectionReason = "existing MAIN node (avoid failover)"
		log.Printf("✅ Keeping existing MAIN node: %s", existingMain.Name)

		// Update target main index to match existing main
		existingMainIndex := c.config.ExtractPodIndex(existingMain.Name)
		if existingMainIndex >= 0 && existingMainIndex <= 1 {
			clusterState.TargetMainIndex = existingMainIndex
			log.Printf("Updated target main index to match existing: %d", existingMainIndex)
		}

		// Priority 3: SYNC replica (guaranteed data consistency)
	} else if syncReplica != nil && c.isPodHealthyForMain(syncReplica) {
		selectedMain = syncReplica
		selectionReason = "SYNC replica promotion (guaranteed consistency)"
		log.Printf("🔄 PROMOTING SYNC REPLICA: %s has all committed transactions", syncReplica.Name)

		// Update target main index to match SYNC replica
		syncReplicaIndex := c.config.ExtractPodIndex(syncReplica.Name)
		if syncReplicaIndex >= 0 && syncReplicaIndex <= 1 {
			clusterState.TargetMainIndex = syncReplicaIndex
			log.Printf("Updated target main index to match SYNC replica: %d", syncReplicaIndex)
		}

		// Priority 4: No safe automatic promotion available
	} else {
		c.handleNoSafePromotion(clusterState, targetMainName, existingMain, syncReplica)
		return
	}

	// Set the selected main
	if selectedMain != nil {
		clusterState.CurrentMain = selectedMain.Name
		log.Printf("✅ Enhanced main selection result: %s (reason: %s)",
			selectedMain.Name, selectionReason)
	}

	// Create and log main selection metrics
	metrics := &MainSelectionMetrics{
		Timestamp:            time.Now(),
		StateType:            clusterState.StateType,
		TargetMainIndex:      clusterState.TargetMainIndex,
		SelectedMain:         clusterState.CurrentMain,
		SelectionReason:      selectionReason,
		HealthyPodsCount:     c.countHealthyPods(clusterState),
		SyncReplicaAvailable: syncReplica != nil,
		FailoverDetected:     false,
		DecisionFactors:      c.buildDecisionFactors(targetMain, existingMain, syncReplica),
	}

	clusterState.LogMainSelectionDecision(metrics)
}

// countHealthyPods counts pods that are healthy for main role
func (c *MemgraphController) countHealthyPods(clusterState *ClusterState) int {
	count := 0
	for _, podInfo := range clusterState.Pods {
		if c.isPodHealthyForMain(podInfo) {
			count++
		}
	}
	return count
}

// buildDecisionFactors creates a list of factors that influenced main selection
func (c *MemgraphController) buildDecisionFactors(targetMain, existingMain, syncReplica *PodInfo) []string {
	factors := []string{}

	if targetMain != nil {
		if c.isPodHealthyForMain(targetMain) {
			factors = append(factors, "target_main_healthy")
		} else {
			factors = append(factors, "target_main_unhealthy")
		}
	} else {
		factors = append(factors, "target_main_missing")
	}

	if existingMain != nil {
		if c.isPodHealthyForMain(existingMain) {
			factors = append(factors, "existing_main_healthy")
		} else {
			factors = append(factors, "existing_main_unhealthy")
		}
	} else {
		factors = append(factors, "no_existing_main")
	}

	if syncReplica != nil {
		if c.isPodHealthyForMain(syncReplica) {
			factors = append(factors, "sync_replica_healthy")
		} else {
			factors = append(factors, "sync_replica_unhealthy")
		}
	} else {
		factors = append(factors, "no_sync_replica")
	}

	return factors
}

// isPodHealthyForMain determines if a pod is healthy enough to be main
func (c *MemgraphController) isPodHealthyForMain(podInfo *PodInfo) bool {
	if podInfo == nil {
		return false
	}

	// Pod must have a Bolt address for client connections
	if podInfo.BoltAddress == "" {
		log.Printf("Pod %s not healthy for main: no bolt address", podInfo.Name)
		return false
	}

	// Pod must have Memgraph role information
	if podInfo.MemgraphRole == "" {
		log.Printf("Pod %s not healthy for main: no Memgraph role info", podInfo.Name)
		return false
	}

	return true
}

// handleNoSafePromotion handles cases where no safe automatic promotion is available
func (c *MemgraphController) handleNoSafePromotion(clusterState *ClusterState, targetMainName string, existingMain, syncReplica *PodInfo) {
	log.Printf("❌ CRITICAL: No safe automatic main promotion available")

	// Analyze why promotion is not safe
	if targetMain, exists := clusterState.Pods[targetMainName]; exists {
		if !c.isPodHealthyForMain(targetMain) {
			log.Printf("Target main %s is not healthy", targetMainName)
		}
	} else {
		log.Printf("Target main %s not found in cluster", targetMainName)
	}

	if existingMain != nil && !c.isPodHealthyForMain(existingMain) {
		log.Printf("Existing main %s is not healthy", existingMain.Name)
	}

	if syncReplica != nil && !c.isPodHealthyForMain(syncReplica) {
		log.Printf("SYNC replica %s is not healthy", syncReplica.Name)
	} else if syncReplica == nil {
		log.Printf("No SYNC replica available for safe promotion")
	}

	// Provide recovery guidance
	log.Printf("🔧 Manual intervention required:")
	log.Printf("  1. Check pod health: kubectl get pods -n %s", c.config.Namespace)
	log.Printf("  2. Check Memgraph connectivity to pods")
	log.Printf("  3. Manually promote healthy pod if available")
	log.Printf("  4. Consider emergency ASYNC→SYNC promotion if needed")

	// Do not set any main - require manual intervention
	clusterState.CurrentMain = ""
}

// validateMainSelection validates the main selection result
func (c *MemgraphController) validateMainSelection(clusterState *ClusterState) {
	log.Printf("Validating main selection result...")

	if clusterState.CurrentMain == "" {
		log.Printf("⚠️  No main selected - cluster will require manual intervention")
		return
	}

	// Validate selected main exists in pods
	mainPod, exists := clusterState.Pods[clusterState.CurrentMain]
	if !exists {
		log.Printf("❌ VALIDATION FAILED: Selected main %s not found in pods", clusterState.CurrentMain)
		clusterState.CurrentMain = ""
		return
	}

	// Validate main health
	if !c.isPodHealthyForMain(mainPod) {
		log.Printf("❌ VALIDATION FAILED: Selected main %s is not healthy", clusterState.CurrentMain)
		clusterState.CurrentMain = ""
		return
	}

	// Validate main index consistency
	mainIndex := c.config.ExtractPodIndex(clusterState.CurrentMain)
	if mainIndex != clusterState.TargetMainIndex {
		log.Printf("⚠️  Main index mismatch: selected=%d, target=%d", mainIndex, clusterState.TargetMainIndex)
		if mainIndex >= 0 && mainIndex <= 1 {
			// Update target to match reality
			clusterState.TargetMainIndex = mainIndex
			log.Printf("Updated target main index to match selection: %d", mainIndex)
		}
	}

	log.Printf("✅ Main selection validation passed: %s (index %d)",
		clusterState.CurrentMain, clusterState.TargetMainIndex)
}

// enforceExpectedTopology enforces expected topology against state drift
func (c *MemgraphController) enforceExpectedTopology(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Enforcing expected topology against state drift...")

	// Reclassify current state to detect drift
	currentStateType := clusterState.ClassifyClusterState()

	// Check for problematic states that need correction
	switch currentStateType {
	case SPLIT_BRAIN_STATE:
		log.Printf("Split-brain detected during operational phase - applying resolution")
		return c.resolveSplitBrain(ctx, clusterState)

	case MIXED_STATE:
		log.Printf("Mixed state detected during operational phase - enforcing known topology")
		return c.enforceKnownTopology(ctx, clusterState)

	case NO_MAIN_STATE:
		log.Printf("No main detected - promoting expected main")
		return c.promoteExpectedMain(ctx, clusterState)

	case OPERATIONAL_STATE:
		log.Printf("Cluster in healthy operational state")
		return nil

	case INITIAL_STATE:
		log.Printf("Cluster reverted to initial state - reapplying deterministic roles")
		c.applyDeterministicRoles(clusterState)
		return nil

	default:
		log.Printf("Unknown cluster state during operational phase: %s", currentStateType.String())
		return nil
	}
}

// resolveSplitBrain resolves split-brain scenarios using lower-index precedence
func (c *MemgraphController) resolveSplitBrain(ctx context.Context, clusterState *ClusterState) error {
	mainPods := clusterState.GetMainPods()
	log.Printf("Resolving split-brain: multiple mains detected: %v", mainPods)

	// Apply lower-index precedence rule
	expectedMainName := c.config.GetPodName(clusterState.TargetMainIndex)

	// Demote all mains except the expected one
	var demotionErrors []error
	for _, mainPod := range mainPods {
		if mainPod != expectedMainName {
			log.Printf("Demoting incorrect main: %s (expected: %s)", mainPod, expectedMainName)

			if podInfo, exists := clusterState.Pods[mainPod]; exists {
				if c.memgraphClient != nil {
					err := c.memgraphClient.SetReplicationRoleToReplicaWithRetry(ctx, podInfo.BoltAddress)
					if err != nil {
						log.Printf("Failed to demote incorrect main %s: %v", mainPod, err)
						demotionErrors = append(demotionErrors, fmt.Errorf("demote %s: %w", mainPod, err))
					} else {
						log.Printf("Successfully demoted incorrect main: %s", mainPod)
					}
				} else {
					log.Printf("Skipping demotion of %s: memgraph client not initialized", mainPod)
				}
			}
		}
	}

	if len(demotionErrors) > 0 {
		return fmt.Errorf("split-brain resolution had %d errors: %v", len(demotionErrors), demotionErrors)
	}

	log.Printf("Split-brain resolved: %s remains as main", expectedMainName)
	return nil
}

// enforceKnownTopology enforces the controller's known topology
func (c *MemgraphController) enforceKnownTopology(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Enforcing known topology: target main index %d", clusterState.TargetMainIndex)

	expectedMainName := c.config.GetPodName(clusterState.TargetMainIndex)
	clusterState.CurrentMain = expectedMainName

	log.Printf("Enforcing expected main: %s", expectedMainName)
	return nil
}

// promoteExpectedMain promotes the expected main when no mains exist
func (c *MemgraphController) promoteExpectedMain(ctx context.Context, clusterState *ClusterState) error {
	expectedMainName := c.config.GetPodName(clusterState.TargetMainIndex)

	log.Printf("Promoting expected main: %s", expectedMainName)

	if podInfo, exists := clusterState.Pods[expectedMainName]; exists {
		if c.memgraphClient != nil {
			err := c.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, podInfo.BoltAddress)
			if err != nil {
				return fmt.Errorf("failed to promote expected main %s: %w", expectedMainName, err)
			}
		} else {
			log.Printf("Skipping promotion of %s: memgraph client not initialized", expectedMainName)
		}

		clusterState.CurrentMain = expectedMainName
		log.Printf("Successfully promoted expected main: %s", expectedMainName)
		return nil
	}

	return fmt.Errorf("expected main pod %s not found", expectedMainName)
}
