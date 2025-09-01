package controller

import (
	"context"
	"fmt"
	"log"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReconcileActions implements DESIGN.md "Reconcile Actions" (lines 75-100) exactly
// This replaces the complex event-driven reconciliation logic with deterministic 8-step process
type ReconcileActions struct {
	controller *MemgraphController
	cluster    *MemgraphCluster
	
	// Failover state tracking
	flipTargets bool  // Set to true when failover flips the target pod roles
	newTargetMainIndex int // The new target main index after failover
}

// NewReconcileActions creates a new ReconcileActions instance
func NewReconcileActions(controller *MemgraphController, cluster *MemgraphCluster) *ReconcileActions {
	return &ReconcileActions{
		controller: controller,
		cluster:    cluster,
	}
}

// ExecuteReconcileActions implements DESIGN.md Reconcile Actions steps 1-8 exactly
func (r *ReconcileActions) ExecuteReconcileActions(ctx context.Context) error {
	log.Println("Starting DESIGN.md compliant reconcile actions...")

	// Step 1: Call kubernetes api to list all memgraph pods, along with their kubernetes status (ready or not)
	podList, err := r.step1_ListMemgraphPods(ctx)
	if err != nil {
		return fmt.Errorf("step 1 failed: %w", err)
	}

	// Get target pods based on DESIGN.md authority model (pod-0 = main, pod-1 = sync)
	targetMainPod, targetSyncReplica := r.getTargetPods(podList)
	if targetMainPod == nil {
		return fmt.Errorf("target main pod not found in cluster")
	}

	// Step 2: If TargetMainPod is not ready, attempt to perform actions in section "Failover Actions"
	// DESIGN.md: "Only continue if 'Failover Actions' succeeded"
	if !r.isPodReady(targetMainPod) {
		log.Printf("Target main pod %s is not ready, performing failover actions...", targetMainPod.Name)
		if err := r.performFailoverActions(ctx, podList); err != nil {
			log.Printf("❌ Failover actions failed: %v", err)
			return fmt.Errorf("failover actions failed, stopping reconciliation: %w", err)
		}
		
		// Failover succeeded - now we need to use the FLIPPED targets
		// After failover, the original TargetSyncReplica is now the TargetMainPod
		if r.flipTargets {
			log.Println("Failover succeeded - continuing reconciliation with flipped target pods")
			// Re-get the target pods with the NEW authority model
			targetMainPod, targetSyncReplica = r.getTargetPodsWithIndex(podList, r.newTargetMainIndex)
			if targetMainPod == nil {
				return fmt.Errorf("new target main pod not found after failover")
			}
			log.Printf("Continuing with new TargetMainPod: %s, new TargetSyncReplica: %s", 
				targetMainPod.Name, 
				func() string {
					if targetSyncReplica != nil {
						return targetSyncReplica.Name
					}
					return "none"
				}())
		}
	}

	// Step 3: Run SHOW REPLICAS to TargetMainPod to get registered replications
	replicatList, err := r.step3_ShowReplicas(ctx, targetMainPod)
	if err != nil {
		return fmt.Errorf("step 3 failed: %w", err)
	}

	// Step 4: If data_info of TargetSyncReplica is not "ready", drop the replication
	if targetSyncReplica != nil {
		if err := r.step4_CheckSyncReplicaDataInfo(ctx, targetMainPod, targetSyncReplica, replicatList); err != nil {
			return fmt.Errorf("step 4 failed: %w", err)
		}
	}

	// Step 5: If pod status of TargetSyncReplica is not "ready", log warning
	if targetSyncReplica != nil {
		r.step5_CheckSyncReplicaPodStatus(targetSyncReplica)
	}

	// Step 6: If data_info for any ASYNC replica is not ready, drop the replication
	if err := r.step6_CheckAsyncReplicasDataInfo(ctx, targetMainPod, replicatList); err != nil {
		return fmt.Errorf("step 6 failed: %w", err)
	}

	// Step 6.5: Ensure SYNC replica relationship exists between target main and target sync replica
	log.Printf("DEBUG: About to check step6_5 condition: targetSyncReplica=%v", targetSyncReplica != nil)
	if targetSyncReplica != nil {
		log.Printf("DEBUG: targetSyncReplica is not nil, calling step6_5...")
		if err := r.step6_5_EnsureSyncReplicaRelationship(ctx, targetMainPod, targetSyncReplica, replicatList); err != nil {
			return fmt.Errorf("step 6.5 failed: %w", err)
		}
	} else {
		log.Printf("DEBUG: targetSyncReplica is nil, skipping step6_5")
	}

	// Step 7: If replication for any pod outside pod-0/pod-1 is missing, handle it
	if err := r.step7_HandleMissingReplications(ctx, targetMainPod, podList, replicatList); err != nil {
		return fmt.Errorf("step 7 failed: %w", err)
	}

	// Step 8: Run SHOW REPLICAS to check final result
	if err := r.step8_ValidateFinalResult(ctx, targetMainPod); err != nil {
		return fmt.Errorf("step 8 failed: %w", err)
	}

	log.Println("DESIGN.md compliant reconcile actions completed successfully")
	return nil
}

// step1_ListMemgraphPods implements Step 1: Call kubernetes api to list all memgraph pods
func (r *ReconcileActions) step1_ListMemgraphPods(ctx context.Context) ([]v1.Pod, error) {
	log.Println("Step 1: Listing all memgraph pods with kubernetes status...")

	pods, err := r.controller.clientset.CoreV1().Pods(r.controller.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=memgraph",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list memgraph pods: %w", err)
	}

	log.Printf("Step 1 result: Found %d memgraph pods", len(pods.Items))
	for _, pod := range pods.Items {
		ready := r.isPodReady(&pod)
		log.Printf("  - Pod %s: ready=%v", pod.Name, ready)
	}

	return pods.Items, nil
}

// step3_ShowReplicas implements Step 3: Run SHOW REPLICAS to TargetMainPod
func (r *ReconcileActions) step3_ShowReplicas(ctx context.Context, targetMainPod *v1.Pod) (map[string]ReplicaInfo, error) {
	log.Printf("Step 3: Running SHOW REPLICAS on target main pod %s...", targetMainPod.Name)

	// Get pod address for Memgraph connection
	podAddress := fmt.Sprintf("%s.%s.%s.svc.cluster.local:7687",
		targetMainPod.Name,
		r.controller.config.StatefulSetName,
		r.controller.config.Namespace)

	replicasResponse, err := r.controller.memgraphClient.QueryReplicasWithRetry(ctx, podAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to query replicas from %s: %w", targetMainPod.Name, err)
	}

	// Convert ReplicasResponse to map[string]ReplicaInfo
	replicas := make(map[string]ReplicaInfo)
	for _, replica := range replicasResponse.Replicas {
		replicas[replica.Name] = replica
	}

	log.Printf("Step 3 result: Found %d registered replicas", len(replicas))
	for name, replica := range replicas {
		log.Printf("  - Replica %s: sync_mode=%s, data_info=%s", name, replica.SyncMode, replica.DataInfo)
	}

	return replicas, nil
}

// step4_CheckSyncReplicaDataInfo implements Step 4: Check SYNC replica data_info
func (r *ReconcileActions) step4_CheckSyncReplicaDataInfo(ctx context.Context, targetMainPod *v1.Pod, targetSyncReplica *v1.Pod, replicatList map[string]ReplicaInfo) error {
	log.Printf("Step 4: Checking data_info of target sync replica %s...", targetSyncReplica.Name)

	// Find the sync replica in the replication list
	syncReplicaName := r.getReplicaNameFromPod(targetSyncReplica)
	if replica, exists := replicatList[syncReplicaName]; exists {
		if replica.SyncMode == "sync" && !r.isDataInfoReady(replica.DataInfo) {
			log.Printf("Step 4: SYNC replica %s data_info is not ready (%s), dropping replication...", 
				targetSyncReplica.Name, replica.DataInfo)
			
			if err := r.dropReplica(ctx, targetMainPod, syncReplicaName); err != nil {
				return fmt.Errorf("failed to drop SYNC replica %s: %w", syncReplicaName, err)
			}
			log.Printf("Step 4: Successfully dropped SYNC replica %s", syncReplicaName)
		} else {
			log.Printf("Step 4: SYNC replica %s data_info is ready", targetSyncReplica.Name)
		}
	} else {
		log.Printf("Step 4: SYNC replica %s not found in replication list", targetSyncReplica.Name)
	}

	return nil
}

// step5_CheckSyncReplicaPodStatus implements Step 5: Check SYNC replica pod status
func (r *ReconcileActions) step5_CheckSyncReplicaPodStatus(targetSyncReplica *v1.Pod) {
	log.Printf("Step 5: Checking pod status of target sync replica %s...", targetSyncReplica.Name)

	if !r.isPodReady(targetSyncReplica) {
		log.Printf("⚠️  WARNING: Target sync replica pod %s is not ready", targetSyncReplica.Name)
	} else {
		log.Printf("Step 5: Target sync replica pod %s is ready", targetSyncReplica.Name)
	}
}

// step6_CheckAsyncReplicasDataInfo implements Step 6: Check ASYNC replicas data_info
func (r *ReconcileActions) step6_CheckAsyncReplicasDataInfo(ctx context.Context, targetMainPod *v1.Pod, replicatList map[string]ReplicaInfo) error {
	log.Println("Step 6: Checking data_info for all ASYNC replicas...")

	droppedCount := 0
	for replicaName, replica := range replicatList {
		if replica.SyncMode == "async" && !r.isDataInfoReady(replica.DataInfo) {
			log.Printf("Step 6: ASYNC replica %s data_info is not ready (%s), dropping replication...", 
				replicaName, replica.DataInfo)
			
			if err := r.dropReplica(ctx, targetMainPod, replicaName); err != nil {
				return fmt.Errorf("failed to drop ASYNC replica %s: %w", replicaName, err)
			}
			droppedCount++
			log.Printf("Step 6: Successfully dropped ASYNC replica %s", replicaName)
		}
	}

	log.Printf("Step 6 completed: Dropped %d unhealthy ASYNC replicas", droppedCount)
	return nil
}

// step6_5_EnsureSyncReplicaRelationship ensures the SYNC replica relationship exists between target main and target sync replica
func (r *ReconcileActions) step6_5_EnsureSyncReplicaRelationship(ctx context.Context, targetMainPod *v1.Pod, targetSyncReplica *v1.Pod, replicatList map[string]ReplicaInfo) error {
	log.Printf("Step 6.5: Ensuring SYNC replica relationship between %s (main) and %s (sync)...", targetMainPod.Name, targetSyncReplica.Name)

	syncReplicaName := r.getReplicaNameFromPod(targetSyncReplica)
	
	// Check if SYNC replica relationship already exists
	if replica, exists := replicatList[syncReplicaName]; exists && replica.SyncMode == "sync" {
		log.Printf("Step 6.5: SYNC replica relationship already exists for %s", targetSyncReplica.Name)
		return nil
	}

	// SYNC replica relationship is missing - need to establish it
	log.Printf("Step 6.5: SYNC replica relationship missing, establishing it...")
	
	// First ensure the target sync replica is actually a replica (not main)
	if err := r.ensurePodIsReplica(ctx, targetSyncReplica); err != nil {
		return fmt.Errorf("failed to ensure %s is replica: %w", targetSyncReplica.Name, err)
	}
	
	// Register the SYNC replica relationship
	if err := r.registerSyncReplica(ctx, targetMainPod, targetSyncReplica); err != nil {
		return fmt.Errorf("failed to register SYNC replica %s: %w", targetSyncReplica.Name, err)
	}
	
	log.Printf("Step 6.5: Successfully established SYNC replica relationship: %s → %s", targetMainPod.Name, targetSyncReplica.Name)
	return nil
}

// step7_HandleMissingReplications implements Step 7: Handle missing replications for pods outside pod-0/pod-1
func (r *ReconcileActions) step7_HandleMissingReplications(ctx context.Context, targetMainPod *v1.Pod, podList []v1.Pod, replicatList map[string]ReplicaInfo) error {
	log.Println("Step 7: Handling missing replications for pods outside pod-0/pod-1...")

	registeredCount := 0
	for _, pod := range podList {
		// Skip pod-0 (main) and pod-1 (sync) as per DESIGN.md authority model
		if r.isPod0(&pod) || r.isPod1(&pod) {
			continue
		}

		replicaName := r.getReplicaNameFromPod(&pod)
		if _, exists := replicatList[replicaName]; !exists {
			log.Printf("Step 7: Missing replication for pod %s", pod.Name)

			// Step 7.1: If the pod is not ready, log warning
			if !r.isPodReady(&pod) {
				log.Printf("⚠️  WARNING: Pod %s is not ready, cannot register replication", pod.Name)
				continue
			}

			// Step 7.2: If the pod is ready, check replication role, demote if MAIN
			if err := r.ensurePodIsReplica(ctx, &pod); err != nil {
				log.Printf("Failed to ensure pod %s is replica: %v", pod.Name, err)
				continue
			}

			// Step 7.3: Register ASYNC replica for the pod
			if err := r.registerAsyncReplica(ctx, targetMainPod, &pod); err != nil {
				log.Printf("Failed to register ASYNC replica for pod %s: %v", pod.Name, err)
				continue
			}

			registeredCount++
			log.Printf("Step 7: Successfully registered ASYNC replica for pod %s", pod.Name)
		}
	}

	log.Printf("Step 7 completed: Registered %d missing ASYNC replicas", registeredCount)
	return nil
}

// step8_ValidateFinalResult implements Step 8: Validate final replication result
func (r *ReconcileActions) step8_ValidateFinalResult(ctx context.Context, targetMainPod *v1.Pod) error {
	log.Printf("Step 8: Running final SHOW REPLICAS validation on %s...", targetMainPod.Name)

	// Get final replication state
	finalReplicas, err := r.step3_ShowReplicas(ctx, targetMainPod)
	if err != nil {
		return fmt.Errorf("failed to get final replication state: %w", err)
	}

	// Check SYNC replica data_info
	syncCount := 0
	asyncCount := 0
	for _, replica := range finalReplicas {
		if replica.SyncMode == "sync" {
			syncCount++
			if !r.isDataInfoReady(replica.DataInfo) {
				log.Printf("🔴 BIG ERROR: SYNC replica data_info is not ready: %s", replica.DataInfo)
			} else {
				log.Printf("Step 8: SYNC replica data_info is ready")
			}
		} else if replica.SyncMode == "async" {
			asyncCount++
			if !r.isDataInfoReady(replica.DataInfo) {
				log.Printf("⚠️  WARNING: ASYNC replica data_info is not ready: %s", replica.DataInfo)
			}
		}
	}

	log.Printf("Step 8 completed: Final state has %d SYNC replicas and %d ASYNC replicas", syncCount, asyncCount)
	return nil
}

// Helper methods

func (r *ReconcileActions) getTargetPods(podList []v1.Pod) (*v1.Pod, *v1.Pod) {
	// Get current target main index from ConfigMap
	state, err := r.controller.getStateManager().LoadState(context.Background())
	if err != nil {
		log.Printf("Warning: failed to load state, defaulting to pod-0 as main: %v", err)
		return r.getTargetPodsWithIndex(podList, 0)
	}
	
	return r.getTargetPodsWithIndex(podList, state.TargetMainIndex)
}

func (r *ReconcileActions) getTargetPodsWithIndex(podList []v1.Pod, targetMainIndex int) (*v1.Pod, *v1.Pod) {
	var targetMainPod, targetSyncReplica *v1.Pod
	
	// Determine which pod should be main and which should be sync based on the index
	mainPodName := r.controller.config.GetPodName(targetMainIndex)
	syncPodIndex := 1 - targetMainIndex // If main is 0, sync is 1; if main is 1, sync is 0
	syncPodName := r.controller.config.GetPodName(syncPodIndex)

	for i := range podList {
		pod := &podList[i]
		if pod.Name == mainPodName {
			targetMainPod = pod
		} else if pod.Name == syncPodName {
			targetSyncReplica = pod
		}
	}

	return targetMainPod, targetSyncReplica
}

func (r *ReconcileActions) isPod0(pod *v1.Pod) bool {
	return pod.Name == r.controller.config.GetPodName(0)
}

func (r *ReconcileActions) isPod1(pod *v1.Pod) bool {
	return pod.Name == r.controller.config.GetPodName(1)
}

func (r *ReconcileActions) isPodReady(pod *v1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

func (r *ReconcileActions) getReplicaNameFromPod(pod *v1.Pod) string {
	// Convert pod name to replica name (e.g., memgraph-ha-0 -> memgraph_ha_0)
	return strings.ReplaceAll(pod.Name, "-", "_")
}

func (r *ReconcileActions) isDataInfoReady(dataInfo string) bool {
	// Check if data_info indicates ready state
	return strings.Contains(dataInfo, `"ready"`) || strings.Contains(dataInfo, "ready")
}

func (r *ReconcileActions) dropReplica(ctx context.Context, mainPod *v1.Pod, replicaName string) error {
	mainBoltAddress := fmt.Sprintf("%s.%s.%s.svc.cluster.local:7687",
		mainPod.Name,
		r.controller.config.StatefulSetName,
		r.controller.config.Namespace)

	return r.controller.memgraphClient.DropReplicaWithRetry(ctx, mainBoltAddress, replicaName)
}

func (r *ReconcileActions) ensurePodIsReplica(ctx context.Context, pod *v1.Pod) error {
	podAddress := fmt.Sprintf("%s.%s.%s.svc.cluster.local:7687",
		pod.Name,
		r.controller.config.StatefulSetName,
		r.controller.config.Namespace)

	// Check current replication role
	roleResponse, err := r.controller.memgraphClient.QueryReplicationRoleWithRetry(ctx, podAddress)
	if err != nil {
		return fmt.Errorf("failed to get replication role for %s: %w", pod.Name, err)
	}

	// If pod is MAIN, demote it to REPLICA
	if roleResponse.Role == "main" {
		log.Printf("Pod %s is MAIN, demoting to REPLICA...", pod.Name)
		if err := r.controller.memgraphClient.SetReplicationRoleToReplicaWithRetry(ctx, podAddress); err != nil {
			return fmt.Errorf("failed to demote %s to replica: %w", pod.Name, err)
		}
		log.Printf("Successfully demoted %s to REPLICA", pod.Name)
	}

	return nil
}

func (r *ReconcileActions) registerAsyncReplica(ctx context.Context, mainPod *v1.Pod, replicaPod *v1.Pod) error {
	mainBoltAddress := fmt.Sprintf("%s.%s.%s.svc.cluster.local:7687",
		mainPod.Name,
		r.controller.config.StatefulSetName,
		r.controller.config.Namespace)

	replicaName := r.getReplicaNameFromPod(replicaPod)
	replicaAddress := fmt.Sprintf("%s:10000", replicaPod.Status.PodIP)
	
	return r.controller.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, mainBoltAddress, replicaName, replicaAddress, "ASYNC")
}

func (r *ReconcileActions) registerSyncReplica(ctx context.Context, mainPod *v1.Pod, replicaPod *v1.Pod) error {
	mainBoltAddress := fmt.Sprintf("%s.%s.%s.svc.cluster.local:7687",
		mainPod.Name,
		r.controller.config.StatefulSetName,
		r.controller.config.Namespace)

	replicaName := r.getReplicaNameFromPod(replicaPod)
	replicaAddress := fmt.Sprintf("%s:10000", replicaPod.Status.PodIP)
	
	return r.controller.memgraphClient.RegisterReplicaWithModeAndRetry(ctx, mainBoltAddress, replicaName, replicaAddress, "SYNC")
}

func (r *ReconcileActions) performFailoverActions(ctx context.Context, podList []v1.Pod) error {
	log.Println("=== DESIGN.md Failover Actions Starting ===")
	log.Println("Presumption: TargetMainPod is not ready")
	
	// Get target pods based on current authority model
	targetMainPod, targetSyncReplica := r.getTargetPods(podList)
	
	// Log current targets for clarity
	targetMainName := "unknown"
	if targetMainPod != nil {
		targetMainName = targetMainPod.Name
	}
	targetSyncName := "unknown"
	if targetSyncReplica != nil {
		targetSyncName = targetSyncReplica.Name
	}
	log.Printf("Current TargetMainPod: %s (not ready)", targetMainName)
	log.Printf("Current TargetSyncReplica: %s", targetSyncName)
	
	// DESIGN.md Failover Step 1: Check cluster recoverability
	log.Println("Failover Step 1: Checking if TargetSyncReplica is ready...")
	if targetSyncReplica == nil {
		log.Printf("❌ CRITICAL: TargetSyncReplica (pod-1) not found in cluster")
		return fmt.Errorf("cluster is not recoverable: TargetSyncReplica not found")
	}
	
	if !r.isPodReady(targetSyncReplica) {
		log.Printf("❌ CRITICAL: Both TargetMainPod (%s) and TargetSyncReplica (%s) are not ready", 
			targetMainName, targetSyncName)
		log.Println("This cluster is NOT RECOVERABLE - failover cannot proceed")
		return fmt.Errorf("cluster is not recoverable: both target pods are not ready")
	}
	log.Printf("✅ TargetSyncReplica %s is ready - cluster is recoverable", targetSyncName)
	
	// DESIGN.md Failover Step 2: Gateway disconnects all existing connections
	log.Println("Failover Step 2: Gateway disconnecting all existing connections...")
	// Note: Gateway will detect main change and handle disconnections automatically
	// This is handled by the gateway's connection management when it detects topology changes
	log.Println("Gateway will disconnect connections upon detecting topology change")
	
	// DESIGN.md Failover Step 3: Promote TargetSyncReplica to MAIN
	log.Printf("Failover Step 3: Promoting %s to MAIN...", targetSyncName)
	syncReplicaAddress := fmt.Sprintf("%s.%s.%s.svc.cluster.local:7687",
		targetSyncReplica.Name,
		r.controller.config.StatefulSetName,
		r.controller.config.Namespace)
	
	if err := r.controller.memgraphClient.SetReplicationRoleToMainWithRetry(ctx, syncReplicaAddress); err != nil {
		return fmt.Errorf("failed to promote %s to MAIN: %w", targetSyncName, err)
	}
	log.Printf("✅ Successfully promoted %s to MAIN role", targetSyncName)
	
	// DESIGN.md Failover Step 4: Flip TargetMainPod with TargetSyncReplica
	log.Printf("Failover Step 4: Flipping target pod roles...")
	
	// Get current target main index from ConfigMap
	state, err := r.controller.getStateManager().LoadState(ctx)
	if err != nil {
		return fmt.Errorf("failed to load state for authority flip: %w", err)
	}
	
	currentTargetIndex := state.TargetMainIndex
	var newTargetIndex int
	if currentTargetIndex == 0 {
		newTargetIndex = 1 // pod-0 failed → pod-1 becomes new TargetMainPod
	} else {
		newTargetIndex = 0 // pod-1 failed → pod-0 becomes new TargetMainPod
	}
	
	// Update the TargetMainIndex in ConfigMap to complete the authority flip
	log.Printf("Updating TargetMainIndex: %d → %d", currentTargetIndex, newTargetIndex)
	if err := r.updateTargetMainIndex(ctx, newTargetIndex,
		fmt.Sprintf("DESIGN.md failover: TargetMainPod switched from pod-%d to pod-%d", 
			currentTargetIndex, newTargetIndex)); err != nil {
		return fmt.Errorf("failed to flip target pod roles: %w", err)
	}
	
	log.Printf("✅ Authority flipped: pod-%d is now TargetMainPod, pod-%d is now TargetSyncReplica", 
		newTargetIndex, currentTargetIndex)
	
	// Important: Return the flipped pods to the caller
	// After this function returns, the ExecuteReconcileActions should use the NEW targets
	r.flipTargets = true
	r.newTargetMainIndex = newTargetIndex
	
	log.Println("=== DESIGN.md Failover Actions Completed Successfully ===")
	return nil
}

// updateTargetMainIndex updates the target main index in the ConfigMap
func (r *ReconcileActions) updateTargetMainIndex(ctx context.Context, newIndex int, reason string) error {
	state, err := r.controller.getStateManager().LoadState(ctx)
	if err != nil {
		return fmt.Errorf("failed to load current state: %w", err)
	}
	
	log.Printf("Updating target main index: %d → %d (reason: %s)", state.TargetMainIndex, newIndex, reason)
	state.TargetMainIndex = newIndex
	
	if err := r.controller.getStateManager().SaveState(ctx, state); err != nil {
		return fmt.Errorf("failed to save updated state: %w", err)
	}
	
	return nil
}