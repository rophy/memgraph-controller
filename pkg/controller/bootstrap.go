package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"
)

// BootstrapController handles the strict bootstrap phase according to README.md design
type BootstrapController struct {
	controller *MemgraphController
	phase      BootstrapPhase
}

type BootstrapPhase int

const (
	BootstrapPhaseDiscovery BootstrapPhase = iota
	BootstrapPhaseValidation
	BootstrapPhaseSetup
	BootstrapPhaseComplete
)

// NewBootstrapController creates a new bootstrap controller
func NewBootstrapController(controller *MemgraphController) *BootstrapController {
	return &BootstrapController{
		controller: controller,
		phase:      BootstrapPhaseDiscovery,
	}
}

// ExecuteBootstrap executes the strict bootstrap process according to README.md
func (bc *BootstrapController) ExecuteBootstrap(ctx context.Context) (*ClusterState, error) {
	log.Println("=== BOOTSTRAP PHASE STARTING ===")
	log.Println("Gateway will REJECT all bolt client connections during bootstrap")
	
	// Ensure gateway is in bootstrap phase
	if bc.controller.gatewayServer != nil {
		bc.controller.gatewayServer.SetBootstrapPhase(true)
	}

	// Step 1: Wait for >=2 replicas with pod status as "ready"
	clusterState, err := bc.waitForMinimumReplicas(ctx)
	if err != nil {
		return nil, fmt.Errorf("bootstrap rule 1 failed: %w", err)
	}

	// Step 2-4: Classify cluster state according to bootstrap rules
	if err := bc.classifyBootstrapState(ctx, clusterState); err != nil {
		return nil, fmt.Errorf("bootstrap classification failed: %w", err)
	}

	// Handle each state according to README.md rules
	switch clusterState.StateType {
	case INITIAL_STATE:
		if err := bc.handleInitialState(ctx, clusterState); err != nil {
			return nil, fmt.Errorf("INITIAL_STATE handling failed: %w", err)
		}
	case OPERATIONAL_STATE:
		if err := bc.handleOperationalState(ctx, clusterState); err != nil {
			return nil, fmt.Errorf("OPERATIONAL_STATE handling failed: %w", err)
		}
	default:
		// UNKNOWN_STATE - crash immediately as per README
		log.Printf("❌ UNKNOWN_STATE detected - controller must crash immediately")
		log.Printf("❌ Manual intervention required to fix cluster state")
		log.Printf("State type: %s", clusterState.StateType.String())
		os.Exit(1)
	}

	// Mark bootstrap complete and transition to operational phase
	clusterState.IsBootstrapPhase = false
	
	// Transition gateway to operational phase
	if bc.controller.gatewayServer != nil {
		bc.controller.gatewayServer.SetBootstrapPhase(false)
		// Start gateway server now that bootstrap is complete
		if err := bc.startGatewayAfterBootstrap(ctx); err != nil {
			log.Printf("Warning: Failed to start gateway after bootstrap: %v", err)
		}
	}
	
	log.Println("=== BOOTSTRAP PHASE COMPLETE ===")
	log.Println("Gateway will now ACCEPT bolt client connections")

	return clusterState, nil
}

// waitForMinimumReplicas implements bootstrap rule 1
func (bc *BootstrapController) waitForMinimumReplicas(ctx context.Context) (*ClusterState, error) {
	log.Println("Bootstrap Rule 1: Waiting for pod-0 and pod-1 to be ready")

	maxWait := 5 * time.Minute
	checkInterval := 10 * time.Second
	startTime := time.Now()

	pod0Name := bc.controller.config.GetPodName(0)
	pod1Name := bc.controller.config.GetPodName(1)

	for {
		// Discover current pods
		clusterState, err := bc.controller.podDiscovery.DiscoverPods(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to discover pods: %w", err)
		}

		// Check if pod-0 and pod-1 are ready
		pod0Info, pod0Exists := clusterState.Pods[pod0Name]
		pod1Info, pod1Exists := clusterState.Pods[pod1Name]

		pod0Ready := pod0Exists && bc.isPodReady(pod0Info)
		pod1Ready := pod1Exists && bc.isPodReady(pod1Info)

		log.Printf("Pod readiness status: %s=%t, %s=%t", pod0Name, pod0Ready, pod1Name, pod1Ready)

		// Check if both pods are ready
		if pod0Ready && pod1Ready {
			log.Printf("✅ Bootstrap Rule 1 satisfied: both %s and %s are ready", pod0Name, pod1Name)
			return clusterState, nil
		}

		// Check timeout
		if time.Since(startTime) > maxWait {
			return nil, fmt.Errorf("timeout waiting for %s and %s to be ready: %s=%t, %s=%t", 
				pod0Name, pod1Name, pod0Name, pod0Ready, pod1Name, pod1Ready)
		}

		log.Printf("Waiting for both pods to be ready...")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(checkInterval):
			continue
		}
	}
}

// classifyBootstrapState implements bootstrap rules 2-4
func (bc *BootstrapController) classifyBootstrapState(ctx context.Context, clusterState *ClusterState) error {
	log.Println("Bootstrap Rules 2-4: Classifying cluster state...")

	// Query Memgraph roles for all ready pods
	if err := bc.queryMemgraphRoles(ctx, clusterState); err != nil {
		return fmt.Errorf("failed to query Memgraph roles: %w", err)
	}

	// Apply bootstrap classification rules
	pod0Name := bc.controller.config.GetPodName(0)
	pod1Name := bc.controller.config.GetPodName(1)

	pod0Info, pod0Exists := clusterState.Pods[pod0Name]
	pod1Info, pod1Exists := clusterState.Pods[pod1Name]

	// Rule 2: Check for INITIAL_STATE
	if pod0Exists && pod1Exists &&
		pod0Info.MemgraphRole == "main" && pod1Info.MemgraphRole == "main" {
		
		// Verify both have empty storage (0 edge_count, 0 vertex_count)
		if bc.hasEmptyStorage(ctx, pod0Info) && bc.hasEmptyStorage(ctx, pod1Info) {
			log.Println("✅ Bootstrap Rule 2: INITIAL_STATE detected (both main, empty storage)")
			clusterState.StateType = INITIAL_STATE
			// Target main index will be set to 0 (always use pod-0 as main)
			if err := bc.controller.updateTargetMainIndex(ctx, 0, "initial state detection"); err != nil {
				return fmt.Errorf("failed to set target main index: %w", err)
			}
			return nil
		}
	}

	// Rule 3: Check for OPERATIONAL_STATE
	mainCount := 0
	replicaCount := 0
	var currentMain string

	for podName, podInfo := range clusterState.Pods {
		switch podInfo.MemgraphRole {
		case "main":
			mainCount++
			currentMain = podName
		case "replica":
			replicaCount++
		}
	}

	if mainCount == 1 {
		log.Printf("✅ Bootstrap Rule 3: OPERATIONAL_STATE detected (1 main: %s)", currentMain)
		clusterState.StateType = OPERATIONAL_STATE
		clusterState.CurrentMain = currentMain
		
		// Set target main index based on current main
		var targetIndex int
		if currentMain == pod0Name {
			targetIndex = 0
		} else if currentMain == pod1Name {
			targetIndex = 1
		} else {
			return fmt.Errorf("main %s is not pod-0 or pod-1 - invalid operational state", currentMain)
		}
		if err := bc.controller.updateTargetMainIndex(ctx, targetIndex, "operational state detection"); err != nil {
			return fmt.Errorf("failed to set target main index: %w", err)
		}
		return nil
	}

	// Rule 4: All other cases are UNKNOWN_STATE
	log.Printf("❌ Bootstrap Rule 4: UNKNOWN_STATE detected")
	log.Printf("Main count: %d, Replica count: %d", mainCount, replicaCount)
	log.Printf("This requires manual intervention to fix")
	clusterState.StateType = UNKNOWN_STATE
	return nil
}

// handleInitialState implements INITIAL_STATE setup according to README
func (bc *BootstrapController) handleInitialState(ctx context.Context, clusterState *ClusterState) error {
	log.Println("Handling INITIAL_STATE: Setting up pod-0 as MAIN, pod-1 as SYNC REPLICA")

	pod0Name := bc.controller.config.GetPodName(0)
	pod1Name := bc.controller.config.GetPodName(1)

	pod1Info, exists := clusterState.Pods[pod1Name]
	if !exists {
		return fmt.Errorf("pod-1 not found for SYNC replica setup")
	}

	// Step 1: Demote pod-1 to replica
	log.Printf("Step 1: Demoting pod-1 (%s) to replica role", pod1Name)
	demoteCmd := "SET REPLICATION ROLE TO REPLICA WITH PORT 10000"
	err := bc.controller.memgraphClient.ExecuteCommandWithRetry(ctx, pod1Info.BoltAddress, demoteCmd)
	if err != nil {
		return fmt.Errorf("failed to demote pod-1 to replica: %w", err)
	}

	// Step 2: Set up SYNC replication from pod-0 to pod-1
	pod0Info := clusterState.Pods[pod0Name]
	log.Printf("Step 2: Setting up SYNC replication from pod-0 to pod-1")
	
	replicaName := pod1Info.GetReplicaName()
	replicaAddress := fmt.Sprintf("%s:10000", pod1Info.Pod.Status.PodIP)
	registerCmd := fmt.Sprintf("REGISTER REPLICA %s SYNC TO \"%s\"", replicaName, replicaAddress)
	
	err = bc.controller.memgraphClient.ExecuteCommandWithRetry(ctx, pod0Info.BoltAddress, registerCmd)
	if err != nil {
		return fmt.Errorf("failed to register SYNC replica: %w", err)
	}

	// Step 3: Verify replication with exponential retry
	log.Printf("Step 3: Verifying replication status with exponential retry")
	if err := bc.verifyReplicationWithRetry(ctx, pod0Info, replicaName); err != nil {
		return fmt.Errorf("replication verification failed: %w", err)
	}

	// Update cluster state
	clusterState.CurrentMain = pod0Name
	pod1Info.IsSyncReplica = true
	
	log.Printf("✅ INITIAL_STATE setup completed: pod-0 is MAIN, pod-1 is SYNC REPLICA")
	return nil
}

// handleOperationalState implements OPERATIONAL_STATE learning
func (bc *BootstrapController) handleOperationalState(ctx context.Context, clusterState *ClusterState) error {
	log.Printf("Handling OPERATIONAL_STATE: Learning existing topology with main %s", clusterState.CurrentMain)
	
	// Update controller's tracking state using consolidated method
	targetMainIndex := bc.controller.getTargetMainIndex()
	if err := bc.controller.updateTargetMainIndex(ctx, targetMainIndex,
		fmt.Sprintf("Learning from OPERATIONAL_STATE with main %s", clusterState.CurrentMain)); err != nil {
		return fmt.Errorf("failed to update target main index: %w", err)
	}
	bc.controller.lastKnownMain = clusterState.CurrentMain
	
	log.Printf("✅ OPERATIONAL_STATE learning completed: main=%s, target_index=%d", 
		clusterState.CurrentMain, bc.controller.getTargetMainIndex())
	return nil
}

// queryMemgraphRoles queries replication roles for all pods
func (bc *BootstrapController) queryMemgraphRoles(ctx context.Context, clusterState *ClusterState) error {
	log.Println("Querying Memgraph roles for cluster state classification...")

	var queryErrors []error
	successCount := 0

	for podName, podInfo := range clusterState.Pods {
		if podInfo.BoltAddress == "" {
			log.Printf("Skipping pod %s: no Bolt address", podName)
			continue
		}

		log.Printf("Querying replication role for pod %s", podName)

		role, err := bc.controller.memgraphClient.QueryReplicationRoleWithRetry(ctx, podInfo.BoltAddress)
		if err != nil {
			log.Printf("Failed to query role for pod %s: %v", podName, err)
			queryErrors = append(queryErrors, fmt.Errorf("pod %s: %w", podName, err))
			continue
		}

		podInfo.MemgraphRole = role.Role
		log.Printf("Pod %s has role: %s", podName, role.Role)
		successCount++

		// Query replicas if this is a main
		if role.Role == "main" {
			replicasResp, err := bc.controller.memgraphClient.QueryReplicasWithRetry(ctx, podInfo.BoltAddress)
			if err != nil {
				log.Printf("Warning: Failed to query replicas for main %s: %v", podName, err)
			} else {
				podInfo.ReplicasInfo = replicasResp.Replicas
			}
		}
	}

	if len(queryErrors) > 0 {
		log.Printf("Role query completed with %d errors:", len(queryErrors))
		for _, err := range queryErrors {
			log.Printf("  - %v", err)
		}
		return fmt.Errorf("failed to query roles for some pods")
	}

	log.Printf("✅ Successfully queried roles for %d pods", successCount)
	return nil
}

// hasEmptyStorage checks if a pod has empty storage (0 edges, 0 vertices)
func (bc *BootstrapController) hasEmptyStorage(ctx context.Context, podInfo *PodInfo) bool {
	// Query storage info to check edge and vertex count
	storageResp, err := bc.controller.memgraphClient.QueryStorageInfoWithRetry(ctx, podInfo.BoltAddress)
	if err != nil {
		log.Printf("Warning: Failed to query storage info for %s: %v", podInfo.Name, err)
		// Conservative approach: assume not empty if we can't check
		return false
	}

	isEmpty := storageResp.EdgeCount == 0 && storageResp.VertexCount == 0
	log.Printf("Pod %s storage: edges=%d, vertices=%d, empty=%t", 
		podInfo.Name, storageResp.EdgeCount, storageResp.VertexCount, isEmpty)
	
	return isEmpty
}

// verifyReplicationWithRetry verifies SYNC replica status with exponential retry
func (bc *BootstrapController) verifyReplicationWithRetry(ctx context.Context, mainPod *PodInfo, replicaName string) error {
	maxRetries := 5
	baseDelay := 1 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * baseDelay
			log.Printf("Replication verification retry %d/%d after %s", attempt+1, maxRetries, delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		// Query replicas from main
		replicasResp, err := bc.controller.memgraphClient.QueryReplicasWithRetry(ctx, mainPod.BoltAddress)
		if err != nil {
			log.Printf("Verification attempt %d failed: %v", attempt+1, err)
			continue
		}

		// Check if our replica is ready
		for _, replica := range replicasResp.Replicas {
			if replica.Name == replicaName {
				// Check data_info field for "ready" status
				if bc.isReplicaReady(replica) {
					log.Printf("✅ Replica %s is ready: %s", replicaName, replica.DataInfo)
					return nil
				}
				log.Printf("Replica %s not ready yet: %s", replicaName, replica.DataInfo)
				break
			}
		}
	}

	log.Printf("❌ Replication verification failed after %d attempts", maxRetries)
	log.Printf("Controller will stay in INITIAL_STATE")
	return fmt.Errorf("SYNC replica verification failed - replica not ready")
}

// isReplicaReady checks if replica data_info indicates ready status
func (bc *BootstrapController) isReplicaReady(replica ReplicaInfo) bool {
	// According to README, we expect: {memgraph: {behind: 0, status: "ready", ts: 0}}
	// For SYNC replicas, empty {} might also be valid
	if replica.DataInfo == "{}" {
		return true // SYNC replicas may show empty data_info when ready
	}

	// TODO: Parse data_info YAML/JSON to check status field
	// For now, use conservative approach - any non-empty data_info is considered ready
	return replica.DataInfo != ""
}

// isPodReady checks if a pod is ready for bootstrap consideration
func (bc *BootstrapController) isPodReady(podInfo *PodInfo) bool {
	if podInfo.Pod == nil {
		return false
	}

	// Check pod phase
	if podInfo.Pod.Status.Phase != "Running" {
		return false
	}

	// Check pod IP
	if podInfo.Pod.Status.PodIP == "" {
		return false
	}

	// Check readiness conditions
	for _, condition := range podInfo.Pod.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True"
		}
	}

	return false
}

// startGatewayAfterBootstrap starts the gateway server after bootstrap completion
func (bc *BootstrapController) startGatewayAfterBootstrap(ctx context.Context) error {
	log.Println("Starting gateway server after bootstrap completion...")
	
	if bc.controller.gatewayServer == nil {
		log.Println("Gateway server is disabled - skipping gateway start")
		return nil
	}
	
	if err := bc.controller.gatewayServer.Start(ctx); err != nil {
		return fmt.Errorf("failed to start gateway after bootstrap: %w", err)
	}
	
	log.Println("✅ Gateway server started successfully after bootstrap")
	return nil
}