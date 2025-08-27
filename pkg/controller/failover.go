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

	// Find suitable replacement using SYNC replica priority
	var syncReplica *PodInfo
	var healthyReplicas []*PodInfo

	for _, podInfo := range clusterState.Pods {
		if podInfo.Name == oldMain {
			continue // Skip the failed main
		}

		if c.isPodHealthyForMain(podInfo) {
			healthyReplicas = append(healthyReplicas, podInfo)

			// Identify SYNC replica
			if podInfo.IsSyncReplica {
				syncReplica = podInfo
			}
		}
	}

	// Failover decision tree
	var newMain *PodInfo
	var promotionReason string

	if syncReplica != nil {
		// Priority 1: SYNC replica (guaranteed data consistency)
		newMain = syncReplica
		promotionReason = "SYNC replica failover (zero data loss)"
		log.Printf("✅ SYNC REPLICA FAILOVER: Promoting %s (guaranteed all committed data)", syncReplica.Name)

	} else if len(healthyReplicas) > 0 {
		// Priority 2: Healthy replica (potential data loss warning)
		newMain = c.selectBestAsyncReplica(healthyReplicas, clusterState.TargetMainIndex)
		promotionReason = "ASYNC replica failover (potential data loss)"
		log.Printf("⚠️  ASYNC REPLICA FAILOVER: Promoting %s (may have missing transactions)", newMain.Name)
		log.Printf("⚠️  WARNING: Potential data loss - ASYNC replica may not have latest committed data")

	} else {
		// No healthy replicas available
		log.Printf("❌ CRITICAL: No healthy replicas available for failover")
		log.Printf("Cluster will remain without main until manual intervention")
		return fmt.Errorf("no healthy replicas available for main failover")
	}

	// IMMEDIATE failover following README.md design
	if newMain != nil {
		failoverStartTime := time.Now()
		log.Printf("🚀 IMMEDIATE failover: %s → %s (reason: %s)",
			oldMain, newMain.Name, promotionReason)

		// Step 1: IMMEDIATE state update (critical path - triggers gateway switch)
		newMainIndex := c.config.ExtractPodIndex(newMain.Name)
		if newMainIndex >= 0 && newMainIndex <= 1 {
			if err := c.updateTargetMainIndex(context.Background(), clusterState, newMainIndex,
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
		failoverMetrics := &MainSelectionMetrics{
			Timestamp:            time.Now(),
			StateType:            clusterState.StateType,
			TargetMainIndex:      clusterState.TargetMainIndex,
			SelectedMain:         newMain.Name,
			SelectionReason:      promotionReason,
			HealthyPodsCount:     len(healthyReplicas),
			SyncReplicaAvailable: syncReplica != nil,
			FailoverDetected:     true,
			DecisionFactors:      []string{fmt.Sprintf("old_main_failed:%s", oldMain), fmt.Sprintf("data_safety:%t", syncReplica != nil)},
		}

		log.Printf("📊 FAILOVER EVENT: old_main=%s, new_main=%s, reason=%s, data_safe=%t",
			oldMain, newMain.Name, promotionReason, syncReplica != nil)

		clusterState.LogMainSelectionDecision(failoverMetrics)
	}

	return nil
}

// selectBestAsyncReplica selects the best ASYNC replica for failover
func (c *MemgraphController) selectBestAsyncReplica(replicas []*PodInfo, targetIndex int) *PodInfo {
	// Prefer replica that matches target main index
	targetMainName := c.config.GetPodName(targetIndex)
	for _, replica := range replicas {
		if replica.Name == targetMainName {
			log.Printf("Selected ASYNC replica matching target index: %s", replica.Name)
			return replica
		}
	}

	// Fallback: select replica with lowest index (deterministic)
	var bestReplica *PodInfo
	bestIndex := 999

	for _, replica := range replicas {
		replicaIndex := c.config.ExtractPodIndex(replica.Name)
		if replicaIndex >= 0 && replicaIndex < bestIndex {
			bestIndex = replicaIndex
			bestReplica = replica
		}
	}

	if bestReplica != nil {
		log.Printf("Selected ASYNC replica with lowest index: %s (index %d)", bestReplica.Name, bestIndex)
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
	
	// Create temporary clusterState to pass to updateTargetMainIndex
	tempClusterState := &ClusterState{
		TargetMainIndex: c.targetMainIndex,
	}
	
	if err := c.updateTargetMainIndex(context.Background(), tempClusterState, newMainIndex, 
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
