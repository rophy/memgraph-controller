package controller

import (
	"context"
	"fmt"
	"log"
	"time"
)

// enhancedMasterSelection applies enhanced master selection logic with SYNC replica priority
func (c *MemgraphController) enhancedMasterSelection(clusterState *ClusterState) {
	log.Printf("Applying enhanced master selection logic...")
	
	// Priority-based master selection strategy:
	// 1. Controller's target master (if available and healthy)
	// 2. Existing MAIN node (avoid unnecessary failover)
	// 3. SYNC replica (guaranteed data consistency)
	// 4. Manual intervention required (no safe automatic promotion)
	
	targetMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	var targetMaster *PodInfo
	var existingMain *PodInfo
	var syncReplica *PodInfo
	
	// Analyze available pods
	for podName, podInfo := range clusterState.Pods {
		// Priority 1: Controller's target master
		if podName == targetMasterName {
			targetMaster = podInfo
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
	
	// Enhanced master selection decision tree
	var selectedMaster *PodInfo
	var selectionReason string
	
	// Priority 1: Controller's target master (if healthy)
	if targetMaster != nil && c.isPodHealthyForMaster(targetMaster) {
		selectedMaster = targetMaster
		selectionReason = fmt.Sprintf("controller target master (index %d)", clusterState.TargetMasterIndex)
		log.Printf("✅ Using controller's target master: %s", targetMasterName)
		
	// Priority 2: Existing MAIN node (avoid unnecessary failover)
	} else if existingMain != nil && c.isPodHealthyForMaster(existingMain) {
		selectedMaster = existingMain
		selectionReason = "existing MAIN node (avoid failover)"
		log.Printf("✅ Keeping existing MAIN node: %s", existingMain.Name)
		
		// Update target master index to match existing master
		existingMasterIndex := c.config.ExtractPodIndex(existingMain.Name)
		if existingMasterIndex >= 0 && existingMasterIndex <= 1 {
			clusterState.TargetMasterIndex = existingMasterIndex
			log.Printf("Updated target master index to match existing: %d", existingMasterIndex)
		}
		
	// Priority 3: SYNC replica (guaranteed data consistency)
	} else if syncReplica != nil && c.isPodHealthyForMaster(syncReplica) {
		selectedMaster = syncReplica
		selectionReason = "SYNC replica promotion (guaranteed consistency)"
		log.Printf("🔄 PROMOTING SYNC REPLICA: %s has all committed transactions", syncReplica.Name)
		
		// Update target master index to match SYNC replica
		syncReplicaIndex := c.config.ExtractPodIndex(syncReplica.Name)
		if syncReplicaIndex >= 0 && syncReplicaIndex <= 1 {
			clusterState.TargetMasterIndex = syncReplicaIndex
			log.Printf("Updated target master index to match SYNC replica: %d", syncReplicaIndex)
		}
		
	// Priority 4: No safe automatic promotion available
	} else {
		c.handleNoSafePromotion(clusterState, targetMasterName, existingMain, syncReplica)
		return
	}
	
	// Set the selected master
	if selectedMaster != nil {
		clusterState.CurrentMaster = selectedMaster.Name
		log.Printf("✅ Enhanced master selection result: %s (reason: %s)", 
			selectedMaster.Name, selectionReason)
	}
	
	// Create and log master selection metrics
	metrics := &MasterSelectionMetrics{
		Timestamp:            time.Now(),
		StateType:            clusterState.StateType,
		TargetMasterIndex:    clusterState.TargetMasterIndex,
		SelectedMaster:       clusterState.CurrentMaster,
		SelectionReason:      selectionReason,
		HealthyPodsCount:     c.countHealthyPods(clusterState),
		SyncReplicaAvailable: syncReplica != nil,
		FailoverDetected:     false,
		DecisionFactors:      c.buildDecisionFactors(targetMaster, existingMain, syncReplica),
	}
	
	clusterState.LogMasterSelectionDecision(metrics)
}

// countHealthyPods counts pods that are healthy for master role
func (c *MemgraphController) countHealthyPods(clusterState *ClusterState) int {
	count := 0
	for _, podInfo := range clusterState.Pods {
		if c.isPodHealthyForMaster(podInfo) {
			count++
		}
	}
	return count
}

// buildDecisionFactors creates a list of factors that influenced master selection
func (c *MemgraphController) buildDecisionFactors(targetMaster, existingMain, syncReplica *PodInfo) []string {
	factors := []string{}
	
	if targetMaster != nil {
		if c.isPodHealthyForMaster(targetMaster) {
			factors = append(factors, "target_master_healthy")
		} else {
			factors = append(factors, "target_master_unhealthy")
		}
	} else {
		factors = append(factors, "target_master_missing")
	}
	
	if existingMain != nil {
		if c.isPodHealthyForMaster(existingMain) {
			factors = append(factors, "existing_main_healthy")
		} else {
			factors = append(factors, "existing_main_unhealthy")
		}
	} else {
		factors = append(factors, "no_existing_main")
	}
	
	if syncReplica != nil {
		if c.isPodHealthyForMaster(syncReplica) {
			factors = append(factors, "sync_replica_healthy")
		} else {
			factors = append(factors, "sync_replica_unhealthy")
		}
	} else {
		factors = append(factors, "no_sync_replica")
	}
	
	return factors
}

// isPodHealthyForMaster determines if a pod is healthy enough to be master
func (c *MemgraphController) isPodHealthyForMaster(podInfo *PodInfo) bool {
	if podInfo == nil {
		return false
	}
	
	// Pod must have a Bolt address for client connections
	if podInfo.BoltAddress == "" {
		log.Printf("Pod %s not healthy for master: no bolt address", podInfo.Name)
		return false
	}
	
	// Pod must have Memgraph role information
	if podInfo.MemgraphRole == "" {
		log.Printf("Pod %s not healthy for master: no Memgraph role info", podInfo.Name)
		return false
	}
	
	return true
}

// handleNoSafePromotion handles cases where no safe automatic promotion is available
func (c *MemgraphController) handleNoSafePromotion(clusterState *ClusterState, targetMasterName string, existingMain, syncReplica *PodInfo) {
	log.Printf("❌ CRITICAL: No safe automatic master promotion available")
	
	// Analyze why promotion is not safe
	if targetMaster, exists := clusterState.Pods[targetMasterName]; exists {
		if !c.isPodHealthyForMaster(targetMaster) {
			log.Printf("Target master %s is not healthy", targetMasterName)
		}
	} else {
		log.Printf("Target master %s not found in cluster", targetMasterName)
	}
	
	if existingMain != nil && !c.isPodHealthyForMaster(existingMain) {
		log.Printf("Existing main %s is not healthy", existingMain.Name)
	}
	
	if syncReplica != nil && !c.isPodHealthyForMaster(syncReplica) {
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
	
	// Do not set any master - require manual intervention
	clusterState.CurrentMaster = ""
}

// validateMasterSelection validates the master selection result
func (c *MemgraphController) validateMasterSelection(clusterState *ClusterState) {
	log.Printf("Validating master selection result...")
	
	if clusterState.CurrentMaster == "" {
		log.Printf("⚠️  No master selected - cluster will require manual intervention")
		return
	}
	
	// Validate selected master exists in pods
	masterPod, exists := clusterState.Pods[clusterState.CurrentMaster]
	if !exists {
		log.Printf("❌ VALIDATION FAILED: Selected master %s not found in pods", clusterState.CurrentMaster)
		clusterState.CurrentMaster = ""
		return
	}
	
	// Validate master health
	if !c.isPodHealthyForMaster(masterPod) {
		log.Printf("❌ VALIDATION FAILED: Selected master %s is not healthy", clusterState.CurrentMaster)
		clusterState.CurrentMaster = ""
		return
	}
	
	// Validate master index consistency
	masterIndex := c.config.ExtractPodIndex(clusterState.CurrentMaster)
	if masterIndex != clusterState.TargetMasterIndex {
		log.Printf("⚠️  Master index mismatch: selected=%d, target=%d", masterIndex, clusterState.TargetMasterIndex)
		if masterIndex >= 0 && masterIndex <= 1 {
			// Update target to match reality
			clusterState.TargetMasterIndex = masterIndex
			log.Printf("Updated target master index to match selection: %d", masterIndex)
		}
	}
	
	log.Printf("✅ Master selection validation passed: %s (index %d)", 
		clusterState.CurrentMaster, clusterState.TargetMasterIndex)
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
		
	case NO_MASTER_STATE:
		log.Printf("No master detected - promoting expected master")
		return c.promoteExpectedMaster(ctx, clusterState)
		
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
	log.Printf("Resolving split-brain: multiple masters detected: %v", mainPods)
	
	// Apply lower-index precedence rule
	expectedMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	
	// Demote all masters except the expected one
	var demotionErrors []error
	for _, masterPod := range mainPods {
		if masterPod != expectedMasterName {
			log.Printf("Demoting incorrect master: %s (expected: %s)", masterPod, expectedMasterName)
			
			if podInfo, exists := clusterState.Pods[masterPod]; exists {
				if c.memgraphClient != nil {
					err := c.memgraphClient.SetReplicationRoleToReplicaWithRetry(ctx, podInfo.BoltAddress)
					if err != nil {
						log.Printf("Failed to demote incorrect master %s: %v", masterPod, err)
						demotionErrors = append(demotionErrors, fmt.Errorf("demote %s: %w", masterPod, err))
					} else {
						log.Printf("Successfully demoted incorrect master: %s", masterPod)
					}
				} else {
					log.Printf("Skipping demotion of %s: memgraph client not initialized", masterPod)
				}
			}
		}
	}
	
	if len(demotionErrors) > 0 {
		return fmt.Errorf("split-brain resolution had %d errors: %v", len(demotionErrors), demotionErrors)
	}
	
	log.Printf("Split-brain resolved: %s remains as master", expectedMasterName)
	return nil
}

// enforceKnownTopology enforces the controller's known topology
func (c *MemgraphController) enforceKnownTopology(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Enforcing known topology: target master index %d", clusterState.TargetMasterIndex)
	
	expectedMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	clusterState.CurrentMaster = expectedMasterName
	
	log.Printf("Enforcing expected master: %s", expectedMasterName)
	return nil
}

// promoteExpectedMaster promotes the expected master when no masters exist
func (c *MemgraphController) promoteExpectedMaster(ctx context.Context, clusterState *ClusterState) error {
	expectedMasterName := c.config.GetPodName(clusterState.TargetMasterIndex)
	
	log.Printf("Promoting expected master: %s", expectedMasterName)
	
	if podInfo, exists := clusterState.Pods[expectedMasterName]; exists {
		if c.memgraphClient != nil {
			err := c.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, podInfo.BoltAddress)
			if err != nil {
				return fmt.Errorf("failed to promote expected master %s: %w", expectedMasterName, err)
			}
		} else {
			log.Printf("Skipping promotion of %s: memgraph client not initialized", expectedMasterName)
		}
		
		clusterState.CurrentMaster = expectedMasterName
		log.Printf("Successfully promoted expected master: %s", expectedMasterName)
		return nil
	}
	
	return fmt.Errorf("expected master pod %s not found", expectedMasterName)
}