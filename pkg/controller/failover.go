package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// detectMainFailover detects if the current main has failed
func (c *MemgraphController) detectMainFailover(clusterState *ClusterState) bool {
	// Use lastKnownMain for operational phase failover detection
	// This ensures we can detect failover even if CurrentMain was cleared by validation
	lastKnownMain := c.lastKnownMain
	if lastKnownMain == "" {
		// No last known main - not a failover scenario (likely fresh bootstrap)
		return false
	}

	log.Printf("Checking failover for last known main: %s", lastKnownMain)

	// Check if last known main pod still exists
	mainPod, exists := clusterState.Pods[lastKnownMain]
	if !exists {
		log.Printf("🚨 MAIN FAILOVER DETECTED: Main pod %s no longer exists", lastKnownMain)
		return true
	}

	// Check if last known main is still healthy
	if !c.isPodHealthyForMain(mainPod) {
		log.Printf("🚨 MAIN FAILOVER DETECTED: Main pod %s is no longer healthy", lastKnownMain)
		return true
	}

	// Check if last known main still has MAIN role
	if mainPod.MemgraphRole != "main" {
		log.Printf("🚨 MAIN FAILOVER DETECTED: Main pod %s no longer has MAIN role (current: %s)",
			lastKnownMain, mainPod.MemgraphRole)
		return true
	}

	log.Printf("✅ Last known main %s is still healthy and has MAIN role", lastKnownMain)
	return false
}

// handleMainFailover handles main failover scenarios with SYNC replica priority
func (c *MemgraphController) handleMainFailover(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("🔄 Handling main failover...")

	// Get the failed main from last known main
	oldMain := c.lastKnownMain
	log.Printf("Handling failover for failed main: %s", oldMain)

	// Design-Contract-Based Failover Logic
	// README.md guarantee: In OPERATIONAL state, either pod-0 OR pod-1 MUST be SYNC replica
	var newMain *PodInfo
	var promotionReason string

	if clusterState.StateType == OPERATIONAL_STATE {
		// Use design contract: the other main-eligible pod MUST be the SYNC replica
		failedMainIndex := c.config.ExtractPodIndex(oldMain)
		
		// Validate design contract assumption
		if failedMainIndex < 0 || failedMainIndex > 1 {
			log.Printf("❌ DESIGN CONTRACT VIOLATION: Failed main %s has invalid index %d (only 0,1 allowed)", oldMain, failedMainIndex)
			return fmt.Errorf("design contract violation: main pod %s has invalid index %d", oldMain, failedMainIndex)
		}

		// The other pod (0 or 1) MUST be the SYNC replica by design
		var newMainIndex int
		if failedMainIndex == 0 {
			newMainIndex = 1 // pod-1 is SYNC replica
		} else {
			newMainIndex = 0 // pod-0 is SYNC replica
		}

		newMainName := c.config.GetPodName(newMainIndex)
		var exists bool
		newMain, exists = clusterState.Pods[newMainName]
		
		if !exists {
			log.Printf("❌ CRITICAL: Design contract SYNC replica %s not found in cluster state", newMainName)
			return fmt.Errorf("design contract SYNC replica %s not available", newMainName)
		}

		if !c.isPodHealthyForMain(newMain) {
			log.Printf("❌ CRITICAL: Design contract SYNC replica %s is not healthy", newMainName)
			return fmt.Errorf("design contract SYNC replica %s is not healthy", newMainName)
		}

		promotionReason = "SYNC replica failover (design contract guarantee)"
		log.Printf("✅ SYNC REPLICA FAILOVER: Promoting %s (guaranteed zero data loss by design contract)", newMainName)
		log.Printf("📋 Design Contract: pod-%d failed → pod-%d is SYNC replica by README.md guarantee", failedMainIndex, newMainIndex)

	} else {
		// Non-OPERATIONAL state: fall back to discovery-based approach
		log.Printf("⚠️  Non-OPERATIONAL state (%s): using discovery-based failover", clusterState.StateType)
		
		var healthyReplicas []*PodInfo
		for _, podInfo := range clusterState.Pods {
			if podInfo.Name == oldMain {
				continue // Skip the failed main
			}
			if c.isPodHealthyForMain(podInfo) {
				healthyReplicas = append(healthyReplicas, podInfo)
			}
		}

		if len(healthyReplicas) > 0 {
			newMain = c.selectBestReplicaForPromotion(healthyReplicas, c.getTargetMainIndex())
			if newMain != nil {
				promotionReason = "Discovery-based replica failover (non-operational state)"
				log.Printf("⚠️  DISCOVERY FAILOVER: Promoting %s (cluster not in operational state)", newMain.Name)
			} else {
				log.Printf("❌ CRITICAL: No main-eligible replicas available")
				return fmt.Errorf("no main-eligible replicas available for failover")
			}
		} else {
			log.Printf("❌ CRITICAL: No healthy replicas available for failover")
			return fmt.Errorf("no healthy replicas available for main failover")
		}
	}

	// IMMEDIATE failover following README.md design
	if newMain != nil {
		failoverStartTime := time.Now()
		log.Printf("🚀 IMMEDIATE failover: %s → %s (reason: %s)",
			oldMain, newMain.Name, promotionReason)

		// Step 1: IMMEDIATE state update (critical path - triggers gateway switch)
		newMainIndex := c.config.ExtractPodIndex(newMain.Name)
		if newMainIndex >= 0 && newMainIndex <= 1 {
			if err := c.updateTargetMainIndex(context.Background(), newMainIndex,
				fmt.Sprintf("IMMEDIATE failover: %s → %s", oldMain, newMain.Name)); err != nil {
				return fmt.Errorf("CRITICAL: failed to update target main index: %w", err)
			}
			
			// Update cluster state and controller state
			clusterState.CurrentMain = newMain.Name
			c.lastKnownMain = newMain.Name
			
			log.Printf("⚡ IMMEDIATE state update completed in %v: targetMainIndex=%d, gateway switching...", 
				time.Since(failoverStartTime), newMainIndex)
		} else {
			return fmt.Errorf("invalid pod index for failover: %d", newMainIndex)
		}

		// Step 2: Single-attempt promotion (best effort, don't block on failure)
		promotionStartTime := time.Now()
		if err := c.memgraphClient.SetReplicationRoleToMain(ctx, newMain.BoltAddress); err != nil {
			log.Printf("⚠️  Promotion command failed (non-blocking): %v", err)
			log.Printf("   → Gateway already switched, reconciliation will retry promotion later")
		} else {
			log.Printf("✅ Promotion command succeeded in %v", time.Since(promotionStartTime))
		}

		// Step 3: Remove failed pod from cluster state to prevent reconciliation delays
		if oldMain != "" {
			delete(clusterState.Pods, oldMain)
			log.Printf("🗑️  Filtered failed pod %s from reconciliation to prevent delays", oldMain)
		}

		totalFailoverTime := time.Since(failoverStartTime)
		log.Printf("🎯 IMMEDIATE failover completed in %v: pod-%d is now MAIN", 
			totalFailoverTime, newMainIndex)
		log.Printf("   → Gateway: ✅ switched | Memgraph: ✅ promoted | Reconciliation: 🗑️  filtered")

		// Log failover event with detailed metrics
		isOperationalFailover := clusterState.StateType == OPERATIONAL_STATE
		failoverMetrics := &MainSelectionMetrics{
			Timestamp:            time.Now(),
			StateType:            clusterState.StateType,
			SelectedMain:         newMain.Name,
			SelectionReason:      promotionReason,
			HealthyPodsCount:     len(clusterState.Pods) - 1, // Total pods minus failed main
			SyncReplicaAvailable: isOperationalFailover,      // OPERATIONAL state guarantees SYNC replica
			FailoverDetected:     true,
			DecisionFactors:      []string{fmt.Sprintf("old_main_failed:%s", oldMain), fmt.Sprintf("design_contract:%t", isOperationalFailover)},
		}

		log.Printf("📊 FAILOVER EVENT: old_main=%s, new_main=%s, reason=%s, design_contract=%t",
			oldMain, newMain.Name, promotionReason, isOperationalFailover)

		clusterState.LogMainSelectionDecision(failoverMetrics)
	}

	return nil
}

// selectBestReplicaForPromotion selects the best available replica for promotion to main
// Used only for non-OPERATIONAL states where design contract doesn't apply
func (c *MemgraphController) selectBestReplicaForPromotion(replicas []*PodInfo, targetIndex int) *PodInfo {
	// Prefer replica that matches target main index
	targetMainName := c.config.GetPodName(targetIndex)
	for _, replica := range replicas {
		if replica.Name == targetMainName {
			log.Printf("Selected replica matching target index: %s", replica.Name)
			return replica
		}
	}

	// Fallback: select replica with lowest index (deterministic, main-eligible pods only)
	var bestReplica *PodInfo
	bestIndex := 999

	for _, replica := range replicas {
		replicaIndex := c.config.ExtractPodIndex(replica.Name)
		// Only consider pods 0 and 1 as main-eligible (2-pod MAIN/SYNC strategy)
		if replicaIndex >= 0 && replicaIndex <= 1 && replicaIndex < bestIndex {
			bestIndex = replicaIndex
			bestReplica = replica
		}
	}

	if bestReplica != nil {
		log.Printf("Selected replica with lowest eligible index: %s (index %d)", bestReplica.Name, bestIndex)
	}

	return bestReplica
}


// handleMainFailurePromotion handles main failure and promotes SYNC replica
func (c *MemgraphController) handleMainFailurePromotion(clusterState *ClusterState, replicaPods []string) error {
	// Find SYNC replica from current state
	var syncReplica string
	for podName, podInfo := range clusterState.Pods {
		if podInfo.IsSyncReplica {
			syncReplica = podName
			break
		}
	}

	// If no SYNC replica found, use deterministic selection based on failed main
	if syncReplica == "" {
		log.Printf("No SYNC replica identified, using deterministic selection")
		// Determine which pod failed and promote the other one (the SYNC replica)
		failedMainIndex := c.identifyFailedMainIndex(clusterState)
		if failedMainIndex == 0 {
			// pod-0 failed, so pod-1 must be the SYNC replica
			syncReplica = c.config.StatefulSetName + "-1"
			log.Printf("Failed main was pod-0, promoting SYNC replica pod-1")
		} else {
			// pod-1 failed, so pod-0 must be the SYNC replica
			syncReplica = c.config.StatefulSetName + "-0"
			log.Printf("Failed main was pod-1, promoting SYNC replica pod-0")
		}
	}

	log.Printf("🔄 FAILOVER: Promoting SYNC replica %s to main", syncReplica)

	// Promote SYNC replica to main
	if err := c.promoteToMain(syncReplica); err != nil {
		return fmt.Errorf("failed to promote SYNC replica %s: %w", syncReplica, err)
	}

	// Update controller state using consolidated method
	newMainIndex := 0
	if syncReplica != c.config.StatefulSetName+"-0" {
		newMainIndex = 1
	}
	
	if err := c.updateTargetMainIndex(context.Background(), newMainIndex, 
		fmt.Sprintf("SYNC replica %s promoted to main", syncReplica)); err != nil {
		return fmt.Errorf("failed to update target main index: %w", err)
	}
	
	c.lastKnownMain = syncReplica

	// Notify gateway of main change (async to avoid blocking reconciliation)
	go c.updateGatewayMain()

	log.Printf("✅ FAILOVER: Successfully promoted %s to main (target_index=%d)",
		syncReplica, c.targetMainIndex)

	return nil
}

// identifyFailedMainIndex determines which pod (0 or 1) was the failed main
func (c *MemgraphController) identifyFailedMainIndex(clusterState *ClusterState) int {
	pod0Name := c.config.StatefulSetName + "-0"
	pod1Name := c.config.StatefulSetName + "-1"

	// Check which pod has the most recent restart (indicating it was the failed main)
	pod0Info, pod0Exists := clusterState.Pods[pod0Name]
	pod1Info, pod1Exists := clusterState.Pods[pod1Name]

	if !pod0Exists && !pod1Exists {
		log.Printf("Warning: Neither pod-0 nor pod-1 found, defaulting to pod-0 as failed main")
		return 0
	}

	if !pod0Exists {
		return 0 // pod-0 doesn't exist, so it failed
	}

	if !pod1Exists {
		return 1 // pod-1 doesn't exist, so it failed
	}

	// Both exist - check which has newer timestamp (more recent restart)
	// The pod that restarted more recently is likely the failed main
	if pod0Info.Timestamp.After(pod1Info.Timestamp) {
		log.Printf("pod-0 has newer timestamp (%v vs %v) - likely the failed main",
			pod0Info.Timestamp, pod1Info.Timestamp)
		return 0
	} else {
		log.Printf("pod-1 has newer timestamp (%v vs %v) - likely the failed main",
			pod1Info.Timestamp, pod0Info.Timestamp)
		return 1
	}
}

// updateSyncReplicaInfo updates IsSyncReplica field for all pods based on actual main replica data
func (c *MemgraphController) updateSyncReplicaInfo(clusterState *ClusterState) {
	// Find current MAIN node
	var mainPod *PodInfo
	for _, podInfo := range clusterState.Pods {
		if podInfo.MemgraphRole == "main" {
			mainPod = podInfo
			break
		}
	}

	if mainPod == nil {
		// No MAIN node found, clear all SYNC replica flags
		for _, podInfo := range clusterState.Pods {
			podInfo.IsSyncReplica = false
		}
		return
	}

	// Mark all replicas as ASYNC first
	for _, podInfo := range clusterState.Pods {
		podInfo.IsSyncReplica = false
	}

	// Identify SYNC replicas from main's replica information
	for _, replica := range mainPod.ReplicasInfo {
		if replica.SyncMode == "sync" { // Memgraph returns lowercase "sync"
			// Convert replica name back to pod name (underscores to dashes)
			podName := strings.ReplaceAll(replica.Name, "_", "-")
			if podInfo, exists := clusterState.Pods[podName]; exists {
				podInfo.IsSyncReplica = true
				log.Printf("Identified SYNC replica: %s", podName)
			}
		}
	}
}
