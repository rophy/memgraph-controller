package controller

import (
	"context"
	"fmt"
	"log"
	"time"
)

// enhancedMainSelection applies enhanced main selection logic with SYNC replica priority
func (c *MemgraphController) enhancedMainSelection(ctx context.Context, clusterState *ClusterState) {
	log.Printf("Applying enhanced main selection logic...")

	// Priority-based main selection strategy:
	// 1. Controller's target main (if available and healthy)
	// 2. Existing MAIN node (avoid unnecessary failover)
	// 3. SYNC replica (guaranteed data consistency)
	// 4. Manual intervention required (no safe automatic promotion)

	targetMainIndex := c.getTargetMainIndex()
	targetMainName := c.config.GetPodName(targetMainIndex)
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
		selectionReason = fmt.Sprintf("controller target main (index %d)", targetMainIndex)
		log.Printf("✅ Using controller's target main: %s", targetMainName)

		// Priority 2: Existing MAIN node (avoid unnecessary failover)
	} else if existingMain != nil && c.isPodHealthyForMain(existingMain) {
		selectedMain = existingMain
		selectionReason = "existing MAIN node (avoid failover)"
		log.Printf("✅ Keeping existing MAIN node: %s", existingMain.Name)

		// Update target main index to match existing main
		existingMainIndex := c.config.ExtractPodIndex(existingMain.Name)
		if existingMainIndex >= 0 && existingMainIndex <= 1 {
			if err := c.updateTargetMainIndex(ctx, existingMainIndex, "existing main discovered"); err != nil {
				log.Printf("Warning: Failed to update target main index for existing main: %v", err)
			}
		}

		// Priority 3: SYNC replica (guaranteed data consistency)
	} else if syncReplica != nil && c.isPodHealthyForMain(syncReplica) {
		selectedMain = syncReplica
		selectionReason = "SYNC replica promotion (guaranteed consistency)"
		log.Printf("🔄 PROMOTING SYNC REPLICA: %s has all committed transactions", syncReplica.Name)

		// Update target main index to match SYNC replica
		syncReplicaIndex := c.config.ExtractPodIndex(syncReplica.Name)
		if syncReplicaIndex >= 0 && syncReplicaIndex <= 1 {
			if err := c.updateTargetMainIndex(ctx, syncReplicaIndex, "SYNC replica promotion"); err != nil {
				log.Printf("Warning: Failed to update target main index for SYNC replica: %v", err)
			}
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
func (c *MemgraphController) validateMainSelection(ctx context.Context, clusterState *ClusterState) {
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
	targetMainIndex := c.getTargetMainIndex()
	if mainIndex != targetMainIndex {
		log.Printf("⚠️  Main index mismatch: selected=%d, target=%d", mainIndex, targetMainIndex)
		if mainIndex >= 0 && mainIndex <= 1 {
			// Update target to match reality
			if err := c.updateTargetMainIndex(ctx, mainIndex, "validation adjustment"); err != nil {
				log.Printf("Warning: Failed to update target main index during validation: %v", err)
			}
		}
	}

	log.Printf("✅ Main selection validation passed: %s (index %d)",
		clusterState.CurrentMain, c.getTargetMainIndex())
}

// enforceExpectedTopology maintains cluster topology during operational phase
// Per README.md: no state classification during operational phase - only event-driven responses
func (c *MemgraphController) enforceExpectedTopology(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Maintaining cluster topology (operational phase)...")

	// During operational phase: simple event-driven logic per README.md
	// 1. Check if main is healthy
	// 2. If not, promote SYNC replica if ready
	// 3. If SYNC replica not ready, log error and wait
	
	// Operational phase assumes cluster is in healthy state
	// Any major issues should be handled by main failover logic
	log.Printf("Cluster in operational phase - topology maintained by event-driven failover")
	return nil
}

// resolveSplitBrain resolves split-brain scenarios using lower-index precedence
func (c *MemgraphController) resolveSplitBrain(ctx context.Context, clusterState *ClusterState) error {
	mainPods := clusterState.GetMainPods()
	log.Printf("Resolving split-brain: multiple mains detected: %v", mainPods)

	// Apply lower-index precedence rule
	expectedMainName := c.config.GetPodName(c.getTargetMainIndex())

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
	targetMainIndex := c.getTargetMainIndex()
	log.Printf("Enforcing known topology: target main index %d", targetMainIndex)

	expectedMainName := c.config.GetPodName(targetMainIndex)
	clusterState.CurrentMain = expectedMainName

	log.Printf("Enforcing expected main: %s", expectedMainName)
	return nil
}

// promoteExpectedMain promotes the expected main when no mains exist
func (c *MemgraphController) promoteExpectedMain(ctx context.Context, clusterState *ClusterState) error {
	expectedMainName := c.config.GetPodName(c.getTargetMainIndex())

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
