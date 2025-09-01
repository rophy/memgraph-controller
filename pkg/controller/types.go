package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type PodState int

const (
	INITIAL PodState = iota // Memgraph is MAIN with no replicas
	MAIN                    // Memgraph role is MAIN with replicas
	REPLICA                 // Memgraph role is REPLICA
)

type ClusterStateType int

const (
	INITIAL_STATE     ClusterStateType = iota // Both pod-0 and pod-1 have MAIN role with 0 edge_count, 0 vertex_count
	OPERATIONAL_STATE                         // One pod is MAIN, the other is REPLICA
	UNKNOWN_STATE                             // Any other scenario - controller should crash
)

func (ps PodState) String() string {
	switch ps {
	case INITIAL:
		return "INITIAL"
	case MAIN:
		return "MAIN"
	case REPLICA:
		return "REPLICA"
	default:
		return "UNKNOWN"
	}
}

func (cst ClusterStateType) String() string {
	switch cst {
	case INITIAL_STATE:
		return "INITIAL_STATE"
	case OPERATIONAL_STATE:
		return "OPERATIONAL_STATE"
	case UNKNOWN_STATE:
		return "UNKNOWN_STATE"
	default:
		return "UNKNOWN_STATE"
	}
}

type ClusterState struct {
	MemgraphNodes        map[string]*MemgraphNode
	CurrentMain string

	// Connection management - integrated with cluster state
	connectionPool *ConnectionPool


	// Controller state tracking
	StateType        ClusterStateType
	IsBootstrapPhase bool // True during initial discovery
	BootstrapSafe    bool // True if bootstrap can proceed safely
	LastStateChange  time.Time
}


func NewClusterState(config *Config) *ClusterState {
	return NewClusterStateWithConnectionPool(config, nil)
}

func NewClusterStateWithConnectionPool(config *Config, connectionPool *ConnectionPool) *ClusterState {
	if connectionPool == nil {
		connectionPool = NewConnectionPool(config)
	}
	return &ClusterState{
		MemgraphNodes:           make(map[string]*MemgraphNode),
		connectionPool: connectionPool,
	}
}





type StateInconsistency struct {
	PodName       string
	MemgraphRole  string
	CurrentState  PodState
	ExpectedState PodState
	ReplicaCount  int
	Description   string
}








// ClassifyClusterState determines the cluster state type based on README.md Bootstrap Phase rules
// This method requires config to calculate pod names properly
// IMPORTANT: This should ONLY be called during BOOTSTRAP phase - crashes if called during operational phase
func (cs *ClusterState) ClassifyClusterState(config *Config) ClusterStateType {
	// Crash if called outside of bootstrap phase - this is a design violation
	if !cs.IsBootstrapPhase {
		panic("CRITICAL: ClassifyClusterState() called during OPERATIONAL phase - this should only be used during BOOTSTRAP phase per README.md design")
	}
	// Only consider pod-0 and pod-1 as per README.md design
	pod0Name := fmt.Sprintf("%s-0", config.StatefulSetName)
	pod1Name := fmt.Sprintf("%s-1", config.StatefulSetName)
	
	pod0, pod0Exists := cs.MemgraphNodes[pod0Name]
	pod1, pod1Exists := cs.MemgraphNodes[pod1Name]
	
	// If either pod-0 or pod-1 is not available, return UNKNOWN_STATE
	if !pod0Exists || !pod1Exists {
		return UNKNOWN_STATE
	}

	// Check if both pods have role information
	if pod0.MemgraphRole == "" || pod1.MemgraphRole == "" {
		return UNKNOWN_STATE
	}

	// Rule 2: Both pod-0 and pod-1 have replication role as MAIN and storage shows 0 edge_count, 0 vertex_count
	if pod0.MemgraphRole == "main" && pod1.MemgraphRole == "main" {
		// TODO: Add storage info check (edge_count, vertex_count) when available
		// For now, assume all dual-main scenarios are fresh clusters (INITIAL_STATE)
		return INITIAL_STATE
	}

	// Rule 3: One pod is MAIN, the other is REPLICA
	if (pod0.MemgraphRole == "main" && pod1.MemgraphRole == "replica") || 
	   (pod0.MemgraphRole == "replica" && pod1.MemgraphRole == "main") {
		return OPERATIONAL_STATE
	}

	// Rule 4: Otherwise, the cluster is in UNKNOWN_STATE
	return UNKNOWN_STATE
}

// IsBootstrapSafe determines if it's safe to proceed during bootstrap
func (cs *ClusterState) IsBootstrapSafe(config *Config) bool {
	stateType := cs.ClassifyClusterState(config)

	switch stateType {
	case INITIAL_STATE, OPERATIONAL_STATE:
		return true
	case UNKNOWN_STATE:
		return false
	default:
		return false
	}
}

// GetMainPods returns list of pods with "main" role
func (cs *ClusterState) GetMainPods() []string {
	var mainPods []string
	for podName, node := range cs.MemgraphNodes {
		if node.MemgraphRole == "main" {
			mainPods = append(mainPods, podName)
		}
	}
	return mainPods
}

// GetReplicaPods returns list of pods with "replica" role
func (cs *ClusterState) GetReplicaPods() []string {
	var replicaPods []string
	for podName, node := range cs.MemgraphNodes {
		if node.MemgraphRole == "replica" {
			replicaPods = append(replicaPods, podName)
		}
	}
	return replicaPods
}

// DetermineMainIndex determines which pod index (0 or 1) should be main
func (cs *ClusterState) DetermineMainIndex(config *Config) (int, error) {
	mainPods := cs.GetMainPods()
	replicaPods := cs.GetReplicaPods()

	// Rule 1: If ALL pods are mains (fresh cluster scenario)
	if len(replicaPods) == 0 && len(mainPods) == len(cs.MemgraphNodes) {
		// Fresh cluster - always choose index 0 as main
		log.Printf("Fresh cluster detected - selecting pod-0 as main")
		return 0, nil
	}

	// Rule 2: If ANY pod is in replica state (existing cluster)
	if len(replicaPods) > 0 {
		return cs.analyzeExistingCluster(mainPods, replicaPods, config)
	}

	// Rule 3: No pods have role information yet
	if len(mainPods) == 0 && len(replicaPods) == 0 {
		// Default to pod-0 when no role information available
		log.Printf("No role information available - defaulting to pod-0 as main")
		return 0, nil
	}

	// Fallback: select pod-0
	return 0, nil
}

// analyzeExistingCluster determines main index for existing clusters
func (cs *ClusterState) analyzeExistingCluster(mainPods, replicaPods []string, config *Config) (int, error) {
	// Look for existing main among eligible pods (pod-0 or pod-1)
	pod0Name := config.GetPodName(0)
	pod1Name := config.GetPodName(1)

	// Check if pod-0 is the current main
	for _, mainPod := range mainPods {
		if mainPod == pod0Name {
			log.Printf("Found existing main pod-0: %s", mainPod)
			return 0, nil
		}
	}

	// Check if pod-1 is the current main
	for _, mainPod := range mainPods {
		if mainPod == pod1Name {
			log.Printf("Found existing main pod-1: %s", mainPod)
			return 1, nil
		}
	}

	// Main is not pod-0 or pod-1 (unusual but possible)
	// Apply lower-index precedence rule
	log.Printf("Current main not in eligible pods (pod-0/pod-1)")
	log.Printf("Current mains: %v", mainPods)
	log.Printf("Applying lower-index precedence rule")

	// Check if pod-0 exists and is available
	if _, exists := cs.MemgraphNodes[pod0Name]; exists {
		log.Printf("Selecting pod-0 as main (lower index precedence)")
		return 0, nil
	}

	// Check if pod-1 exists and is available
	if _, exists := cs.MemgraphNodes[pod1Name]; exists {
		log.Printf("Selecting pod-1 as main (pod-0 not available)")
		return 1, nil
	}

	return -1, fmt.Errorf("neither pod-0 nor pod-1 available for main role")
}

// ValidateControllerState validates the internal controller state consistency
func (cs *ClusterState) ValidateControllerState(config *Config) []string {
	var warnings []string

	// Validate target main index
	// Target main index validation removed - now managed in controller state

	// Validate current main exists in pods
	if cs.CurrentMain != "" {
		if _, exists := cs.MemgraphNodes[cs.CurrentMain]; !exists {
			warnings = append(warnings, fmt.Sprintf("Current main '%s' not found in discovered pods", cs.CurrentMain))
		}
	}

	// Validate state type consistency - only during bootstrap phase
	if cs.IsBootstrapPhase {
		actualStateType := cs.ClassifyClusterState(config)
		if cs.StateType != actualStateType {
			warnings = append(warnings, fmt.Sprintf("State type mismatch: recorded=%s, actual=%s", cs.StateType.String(), actualStateType.String()))
		}
	}

	// Bootstrap phase consistency
	if cs.IsBootstrapPhase && !cs.BootstrapSafe {
		warnings = append(warnings, "Bootstrap phase marked as unsafe - controller should not proceed")
	}

	return warnings
}

// LogStateTransition logs important state changes for debugging
func (cs *ClusterState) LogStateTransition(oldState ClusterStateType, reason string) {
	if oldState != cs.StateType {
		log.Printf("🔄 STATE TRANSITION: %s → %s (reason: %s)",
			oldState.String(), cs.StateType.String(), reason)
		cs.LastStateChange = time.Now()
	}
}

// MainSelectionMetrics tracks main selection decision making
type MainSelectionMetrics struct {
	Timestamp            time.Time
	StateType            ClusterStateType
	// TargetMainIndex removed - now accessed via controller.getTargetMainIndex()
	SelectedMain         string
	SelectionReason      string
	HealthyPodsCount     int
	SyncReplicaAvailable bool
	FailoverDetected     bool
	DecisionFactors      []string
}

// LogMainSelectionDecision logs detailed main selection metrics
func (cs *ClusterState) LogMainSelectionDecision(metrics *MainSelectionMetrics) {
	log.Printf("📊 MAIN SELECTION METRICS:")
	log.Printf("  Timestamp: %s", metrics.Timestamp.Format(time.RFC3339))
	log.Printf("  State Type: %s", metrics.StateType.String())
	// Target main index logging moved to controller
	log.Printf("  Selected Main: %s", metrics.SelectedMain)
	log.Printf("  Selection Reason: %s", metrics.SelectionReason)
	log.Printf("  Healthy Pods: %d", metrics.HealthyPodsCount)
	log.Printf("  SYNC Replica Available: %t", metrics.SyncReplicaAvailable)
	log.Printf("  Failover Detected: %t", metrics.FailoverDetected)
	log.Printf("  Decision Factors: %v", metrics.DecisionFactors)
}

// GetClusterHealthSummary returns a summary of cluster health
func (cs *ClusterState) GetClusterHealthSummary(targetIndex int) map[string]interface{} {
	healthyPods := 0
	totalPods := len(cs.MemgraphNodes)
	syncReplicaCount := 0
	mainPods := 0
	replicaPods := 0

	for _, node := range cs.MemgraphNodes {
		if node.BoltAddress != "" && node.MemgraphRole != "" {
			healthyPods++
		}

		if node.IsSyncReplica {
			syncReplicaCount++
		}

		switch node.MemgraphRole {
		case "main":
			mainPods++
		case "replica":
			replicaPods++
		}
	}

	return map[string]interface{}{
		"total_pods":      totalPods,
		"healthy_pods":    healthyPods,
		"unhealthy_pods":  totalPods - healthyPods,
		"main_pods":       mainPods,
		"replica_pods":    replicaPods,
		"sync_replicas":   syncReplicaCount,
		"current_main":    cs.CurrentMain,
		"target_index":    targetIndex,
		"state_type":      cs.StateType.String(),
		"bootstrap_phase": cs.IsBootstrapPhase,
		"last_change":     cs.LastStateChange,
	}
}


// ReconciliationMetrics tracks reconciliation performance and behavior
type ReconciliationMetrics struct {
	TotalReconciliations      int64         `json:"total_reconciliations"`
	SuccessfulReconciliations int64         `json:"successful_reconciliations"`
	FailedReconciliations     int64         `json:"failed_reconciliations"`
	AverageReconciliationTime time.Duration `json:"average_reconciliation_time"`
	LastReconciliationTime    time.Time     `json:"last_reconciliation_time"`
	LastReconciliationReason  string        `json:"last_reconciliation_reason"`
	LastReconciliationError   string        `json:"last_reconciliation_error,omitempty"`
}

// Connection management methods for ClusterState

// GetDriver gets a Neo4j driver for the specified pod
func (cs *ClusterState) GetDriver(ctx context.Context, podName string) (neo4j.DriverWithContext, error) {
	node, exists := cs.MemgraphNodes[podName]
	if !exists {
		return nil, fmt.Errorf("pod %s not found in cluster state", podName)
	}
	
	if node.BoltAddress == "" {
		return nil, fmt.Errorf("pod %s has no bolt address", podName)
	}
	
	return cs.connectionPool.GetDriver(ctx, node.BoltAddress)
}

// GetDriverByAddress gets a Neo4j driver for the specified bolt address
func (cs *ClusterState) GetDriverByAddress(ctx context.Context, boltAddress string) (neo4j.DriverWithContext, error) {
	if cs.connectionPool == nil {
		return nil, fmt.Errorf("connection pool not initialized")
	}
	return cs.connectionPool.GetDriver(ctx, boltAddress)
}

// InvalidatePodConnection invalidates the connection for a specific pod
func (cs *ClusterState) InvalidatePodConnection(podName string) {
	if cs.connectionPool == nil {
		return
	}
	
	if node, exists := cs.MemgraphNodes[podName]; exists && node.BoltAddress != "" {
		cs.connectionPool.InvalidateConnection(node.BoltAddress)
		log.Printf("Invalidated connection for pod %s (%s)", podName, node.BoltAddress)
	}
}

// HandlePodIPChange handles IP changes for a pod, invalidating old connections
func (cs *ClusterState) HandlePodIPChange(podName, oldIP, newIP string) {
	if cs.connectionPool == nil {
		return
	}
	
	if oldIP != "" && oldIP != newIP {
		oldBoltAddress := oldIP + ":7687"
		cs.connectionPool.InvalidateConnection(oldBoltAddress)
		log.Printf("Invalidated connection for pod %s: IP changed from %s to %s", podName, oldIP, newIP)
	}
	
	// Update the pod info with new IP
	if node, exists := cs.MemgraphNodes[podName]; exists {
		newBoltAddress := ""
		if newIP != "" {
			newBoltAddress = newIP + ":7687"
		}
		node.BoltAddress = newBoltAddress
	}
}

// CloseAllConnections closes all connections in the connection pool
func (cs *ClusterState) CloseAllConnections(ctx context.Context) {
	if cs.connectionPool != nil {
		cs.connectionPool.Close(ctx)
	}
}
