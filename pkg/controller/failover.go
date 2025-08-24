package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// detectMasterFailover detects if the current master has failed
func (c *MemgraphController) detectMasterFailover(clusterState *ClusterState) bool {
	if clusterState.CurrentMaster == "" {
		// No master currently set - not a failover scenario
		return false
	}
	
	// Check if current master pod still exists
	masterPod, exists := clusterState.Pods[clusterState.CurrentMaster]
	if !exists {
		log.Printf("🚨 MASTER FAILOVER DETECTED: Master pod %s no longer exists", clusterState.CurrentMaster)
		return true
	}
	
	// Check if current master is still healthy
	if !c.isPodHealthyForMaster(masterPod) {
		log.Printf("🚨 MASTER FAILOVER DETECTED: Master pod %s is no longer healthy", clusterState.CurrentMaster)
		return true
	}
	
	// Check if current master still has MAIN role
	if masterPod.MemgraphRole != "main" {
		log.Printf("🚨 MASTER FAILOVER DETECTED: Master pod %s no longer has MAIN role (current: %s)", 
			clusterState.CurrentMaster, masterPod.MemgraphRole)
		return true
	}
	
	return false
}

// handleMasterFailover handles master failover scenarios with SYNC replica priority
func (c *MemgraphController) handleMasterFailover(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("🔄 Handling master failover...")
	
	// Clear failed master
	oldMaster := clusterState.CurrentMaster
	clusterState.CurrentMaster = ""
	
	// Find suitable replacement using SYNC replica priority
	var syncReplica *PodInfo
	var healthyReplicas []*PodInfo
	
	for _, podInfo := range clusterState.Pods {
		if podInfo.Name == oldMaster {
			continue // Skip the failed master
		}
		
		if c.isPodHealthyForMaster(podInfo) {
			healthyReplicas = append(healthyReplicas, podInfo)
			
			// Identify SYNC replica
			if podInfo.IsSyncReplica {
				syncReplica = podInfo
			}
		}
	}
	
	// Failover decision tree
	var newMaster *PodInfo
	var promotionReason string
	
	if syncReplica != nil {
		// Priority 1: SYNC replica (guaranteed data consistency)
		newMaster = syncReplica
		promotionReason = "SYNC replica failover (zero data loss)"
		log.Printf("✅ SYNC REPLICA FAILOVER: Promoting %s (guaranteed all committed data)", syncReplica.Name)
		
	} else if len(healthyReplicas) > 0 {
		// Priority 2: Healthy replica (potential data loss warning)
		newMaster = c.selectBestAsyncReplica(healthyReplicas, clusterState.TargetMasterIndex)
		promotionReason = "ASYNC replica failover (potential data loss)"
		log.Printf("⚠️  ASYNC REPLICA FAILOVER: Promoting %s (may have missing transactions)", newMaster.Name)
		log.Printf("⚠️  WARNING: Potential data loss - ASYNC replica may not have latest committed data")
		
	} else {
		// No healthy replicas available
		log.Printf("❌ CRITICAL: No healthy replicas available for failover")
		log.Printf("Cluster will remain without master until manual intervention")
		return fmt.Errorf("no healthy replicas available for master failover")
	}
	
	// Promote new master
	if newMaster != nil {
		log.Printf("🔄 Promoting new master: %s → %s (reason: %s)", 
			oldMaster, newMaster.Name, promotionReason)
		
		// Perform promotion
		err := c.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, newMaster.BoltAddress)
		if err != nil {
			return fmt.Errorf("failed to promote new master %s: %w", newMaster.Name, err)
		}
		
		// Update cluster state
		clusterState.CurrentMaster = newMaster.Name
		
		// Update target master index to match promoted master
		newMasterIndex := c.config.ExtractPodIndex(newMaster.Name)
		if newMasterIndex >= 0 && newMasterIndex <= 1 {
			clusterState.TargetMasterIndex = newMasterIndex
		}
		
		log.Printf("✅ Master failover completed: %s promoted successfully", newMaster.Name)
		
		// Log failover event with detailed metrics
		failoverMetrics := &MasterSelectionMetrics{
			Timestamp:            time.Now(),
			StateType:            clusterState.StateType,
			TargetMasterIndex:    clusterState.TargetMasterIndex,
			SelectedMaster:       newMaster.Name,
			SelectionReason:      promotionReason,
			HealthyPodsCount:     len(healthyReplicas),
			SyncReplicaAvailable: syncReplica != nil,
			FailoverDetected:     true,
			DecisionFactors:      []string{fmt.Sprintf("old_master_failed:%s", oldMaster), fmt.Sprintf("data_safety:%t", syncReplica != nil)},
		}
		
		log.Printf("📊 FAILOVER EVENT: old_master=%s, new_master=%s, reason=%s, data_safe=%t", 
			oldMaster, newMaster.Name, promotionReason, syncReplica != nil)
		
		clusterState.LogMasterSelectionDecision(failoverMetrics)
	}
	
	return nil
}

// selectBestAsyncReplica selects the best ASYNC replica for failover
func (c *MemgraphController) selectBestAsyncReplica(replicas []*PodInfo, targetIndex int) *PodInfo {
	// Prefer replica that matches target master index
	targetMasterName := c.config.GetPodName(targetIndex)
	for _, replica := range replicas {
		if replica.Name == targetMasterName {
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

// selectBestAsyncReplicaForSyncPromotion selects the best ASYNC replica for promotion to SYNC
func (c *MemgraphController) selectBestAsyncReplicaForSyncPromotion(clusterState *ClusterState) string {
	currentMaster := clusterState.CurrentMaster
	targetSyncReplicaName := c.selectSyncReplica(clusterState, currentMaster)
	
	// First preference: the pod that should normally be SYNC replica
	if targetPod, exists := clusterState.Pods[targetSyncReplicaName]; exists {
		if c.isPodHealthyForMaster(targetPod) && targetPod.MemgraphRole == "replica" && !targetPod.IsSyncReplica {
			log.Printf("Best ASYNC→SYNC candidate: %s (normal SYNC replica pod)", targetSyncReplicaName)
			return targetSyncReplicaName
		}
	}
	
	// Second preference: healthy ASYNC replicas, lowest index first
	var candidates []string
	for podName, podInfo := range clusterState.Pods {
		if podName != currentMaster && 
		   !podInfo.IsSyncReplica && 
		   podInfo.MemgraphRole == "replica" && 
		   c.isPodHealthyForMaster(podInfo) {
			candidates = append(candidates, podName)
		}
	}
	
	if len(candidates) == 0 {
		return ""
	}
	
	// Sort by index (deterministic selection)
	var bestCandidate string
	bestIndex := 999
	for _, candidate := range candidates {
		index := c.config.ExtractPodIndex(candidate)
		if index >= 0 && index < bestIndex {
			bestIndex = index
			bestCandidate = candidate
		}
	}
	
	if bestCandidate != "" {
		log.Printf("Best ASYNC→SYNC candidate: %s (lowest index %d)", bestCandidate, bestIndex)
	}
	
	return bestCandidate
}

// handleMasterFailurePromotion handles master failure and promotes SYNC replica
func (c *MemgraphController) handleMasterFailurePromotion(clusterState *ClusterState, replicaPods []string) error {
	// Find SYNC replica from current state
	var syncReplica string
	for podName, podInfo := range clusterState.Pods {
		if podInfo.IsSyncReplica {
			syncReplica = podName
			break
		}
	}
	
	// If no SYNC replica found, use deterministic selection based on failed master
	if syncReplica == "" {
		log.Printf("No SYNC replica identified, using deterministic selection")
		// Determine which pod failed and promote the other one (the SYNC replica)
		failedMasterIndex := c.identifyFailedMasterIndex(clusterState)
		if failedMasterIndex == 0 {
			// pod-0 failed, so pod-1 must be the SYNC replica
			syncReplica = c.config.StatefulSetName + "-1"
			log.Printf("Failed master was pod-0, promoting SYNC replica pod-1")
		} else {
			// pod-1 failed, so pod-0 must be the SYNC replica  
			syncReplica = c.config.StatefulSetName + "-0"
			log.Printf("Failed master was pod-1, promoting SYNC replica pod-0")
		}
	}
	
	log.Printf("🔄 FAILOVER: Promoting SYNC replica %s to master", syncReplica)
	
	// Promote SYNC replica to master
	if err := c.promoteToMaster(syncReplica); err != nil {
		return fmt.Errorf("failed to promote SYNC replica %s: %w", syncReplica, err)
	}
	
	// Update controller state
	c.lastKnownMaster = syncReplica
	if syncReplica == c.config.StatefulSetName + "-0" {
		c.targetMasterIndex = 0
	} else {
		c.targetMasterIndex = 1
	}
	
	// Notify gateway of master change (async to avoid blocking reconciliation)
	go c.updateGatewayMaster()
	
	log.Printf("✅ FAILOVER: Successfully promoted %s to master (target_index=%d)", 
		syncReplica, c.targetMasterIndex)
	
	return nil
}

// identifyFailedMasterIndex determines which pod (0 or 1) was the failed master
func (c *MemgraphController) identifyFailedMasterIndex(clusterState *ClusterState) int {
	pod0Name := c.config.StatefulSetName + "-0"
	pod1Name := c.config.StatefulSetName + "-1"
	
	// Check which pod has the most recent restart (indicating it was the failed master)
	pod0Info, pod0Exists := clusterState.Pods[pod0Name]
	pod1Info, pod1Exists := clusterState.Pods[pod1Name]
	
	if !pod0Exists && !pod1Exists {
		log.Printf("Warning: Neither pod-0 nor pod-1 found, defaulting to pod-0 as failed master")
		return 0
	}
	
	if !pod0Exists {
		return 0 // pod-0 doesn't exist, so it failed
	}
	
	if !pod1Exists {
		return 1 // pod-1 doesn't exist, so it failed
	}
	
	// Both exist - check which has newer timestamp (more recent restart)
	// The pod that restarted more recently is likely the failed master
	if pod0Info.Timestamp.After(pod1Info.Timestamp) {
		log.Printf("pod-0 has newer timestamp (%v vs %v) - likely the failed master", 
			pod0Info.Timestamp, pod1Info.Timestamp)
		return 0
	} else {
		log.Printf("pod-1 has newer timestamp (%v vs %v) - likely the failed master", 
			pod1Info.Timestamp, pod0Info.Timestamp)
		return 1
	}
}

// updateSyncReplicaInfo updates IsSyncReplica field for all pods based on actual master replica data
func (c *MemgraphController) updateSyncReplicaInfo(clusterState *ClusterState) {
	// Find current MAIN node
	var masterPod *PodInfo
	for _, podInfo := range clusterState.Pods {
		if podInfo.MemgraphRole == "main" {
			masterPod = podInfo
			break
		}
	}
	
	if masterPod == nil {
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
	
	// Identify SYNC replicas from master's replica information
	for _, replica := range masterPod.ReplicasInfo {
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