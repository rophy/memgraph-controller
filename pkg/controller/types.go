package controller

import (
	"fmt"
	"log"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
)

type PodState int

const (
	INITIAL PodState = iota // Memgraph is MAIN with no replicas
	MASTER                  // Memgraph role is MAIN with replicas
	REPLICA                 // Memgraph role is REPLICA
)

type ClusterStateType int

const (
	INITIAL_STATE     ClusterStateType = iota // All pods are "main" - fresh cluster
	OPERATIONAL_STATE                         // Exactly one master - healthy state  
	MIXED_STATE                               // Some main, some replica - conflicts
	NO_MASTER_STATE                           // No main pods - requires promotion
	SPLIT_BRAIN_STATE                         // Multiple masters - dangerous
)

func (ps PodState) String() string {
	switch ps {
	case INITIAL:
		return "INITIAL"
	case MASTER:
		return "MASTER"
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
	case MIXED_STATE:
		return "MIXED_STATE"
	case NO_MASTER_STATE:
		return "NO_MASTER_STATE"
	case SPLIT_BRAIN_STATE:
		return "SPLIT_BRAIN_STATE"
	default:
		return "UNKNOWN_STATE"
	}
}

type ClusterState struct {
	Pods          map[string]*PodInfo
	CurrentMaster string
	
	// Controller state tracking
	StateType           ClusterStateType
	TargetMasterIndex   int    // 0 or 1 - which of pod-0/pod-1 should be master
	IsBootstrapPhase    bool   // True during initial discovery
	BootstrapSafe       bool   // True if bootstrap can proceed safely
	LastStateChange     time.Time
}

type PodInfo struct {
	Name                  string
	State                 PodState      // Derived from Memgraph queries only
	Timestamp             time.Time     // Pod creation/restart time
	MemgraphRole          string        // Result of SHOW REPLICATION ROLE ("MAIN", "REPLICA")
	BoltAddress           string        // Pod IP:7687 for Bolt connections
	ReplicationAddress    string        // <pod-name>.<service-name>:10000 for replication
	ReplicaName           string        // Pod name with dashes → underscores for REGISTER REPLICA
	Replicas              []string      // Result of SHOW REPLICAS (only for MAIN nodes)
	ReplicasInfo          []ReplicaInfo // Detailed replica information including sync mode
	IsSyncReplica         bool          // True if this replica is configured as SYNC
	Pod                   *v1.Pod       // Reference to Kubernetes pod object
}

func NewClusterState() *ClusterState {
	return &ClusterState{
		Pods: make(map[string]*PodInfo),
	}
}

func NewPodInfo(pod *v1.Pod, serviceName string) *PodInfo {
	podName := pod.Name
	
	// Extract timestamp (prefer status start time, fallback to creation time)
	timestamp := pod.CreationTimestamp.Time
	if pod.Status.StartTime != nil {
		timestamp = pod.Status.StartTime.Time
	}

	// Build addresses
	boltAddress := ""
	if pod.Status.PodIP != "" {
		boltAddress = pod.Status.PodIP + ":7687"
	}
	
	replicationAddress := podName + "." + serviceName + ":10000"
	
	// Convert pod name for replica registration (dashes to underscores)
	replicaName := convertPodNameForReplica(podName)

	return &PodInfo{
		Name:               podName,
		State:              INITIAL, // Will be determined later
		Timestamp:          timestamp,
		MemgraphRole:       "",      // Will be queried later
		BoltAddress:        boltAddress,
		ReplicationAddress: replicationAddress,
		ReplicaName:        replicaName,
		Replicas:           []string{},
		ReplicasInfo:       []ReplicaInfo{},
		IsSyncReplica:      false,
		Pod:                pod,
	}
}

// convertPodNameForReplica converts pod name to replica name by replacing dashes with underscores
// Example: "memgraph-1" -> "memgraph_1"
func convertPodNameForReplica(podName string) string {
	result := ""
	for _, char := range podName {
		if char == '-' {
			result += "_"
		} else {
			result += string(char)
		}
	}
	return result
}

// ClassifyPodState determines the actual pod state based on Memgraph role and replica configuration
func (pi *PodInfo) ClassifyState() PodState {
	// If we don't have Memgraph role information yet, return current state
	if pi.MemgraphRole == "" {
		return pi.State
	}

	// State classification based on actual Memgraph configuration:
	// - INITIAL: Memgraph role is main with no replicas (standalone)
	// - MASTER: Memgraph role is main with replicas configured
	// - REPLICA: Memgraph role is replica
	
	// Note: We classify based on actual Memgraph state only.

	switch {
	case pi.MemgraphRole == "main" && len(pi.Replicas) == 0:
		return INITIAL
	case pi.MemgraphRole == "main" && len(pi.Replicas) > 0:
		return MASTER
	case pi.MemgraphRole == "replica":
		return REPLICA
	default:
		// Unknown Memgraph role, return current state
		return pi.State
	}
}

// DetectStateInconsistency checks if pod state is inconsistent with Memgraph role
func (pi *PodInfo) DetectStateInconsistency() *StateInconsistency {
	if pi.MemgraphRole == "" {
		// Can't detect inconsistency without Memgraph role info
		return nil
	}

	expectedState := pi.ClassifyState()
	
	// Check if current state matches expected state based on Memgraph role
	if pi.State == expectedState {
		return nil // No inconsistency
	}

	return &StateInconsistency{
		PodName:           pi.Name,
		MemgraphRole:      pi.MemgraphRole,
		CurrentState:      pi.State,
		ExpectedState:     expectedState,
		ReplicaCount:      len(pi.Replicas),
		Description:       buildInconsistencyDescription(pi),
	}
}

type StateInconsistency struct {
	PodName        string
	MemgraphRole   string
	CurrentState   PodState
	ExpectedState  PodState
	ReplicaCount   int
	Description    string
}

func buildInconsistencyDescription(pi *PodInfo) string {
	expectedState := pi.ClassifyState()
	return fmt.Sprintf("Pod state %s does not match expected state %s (Memgraph role: %s, replicas: %d)", 
		pi.State.String(), expectedState.String(), pi.MemgraphRole, len(pi.Replicas))
}

// GetReplicaName converts pod name to replica name (dashes to underscores)
func (pi *PodInfo) GetReplicaName() string {
	return strings.ReplaceAll(pi.Name, "-", "_")
}

// GetReplicationAddress returns the replication address for this pod using DNS name
func (pi *PodInfo) GetReplicationAddress(serviceName string) string {
	return fmt.Sprintf("%s.%s:10000", pi.Name, serviceName)
}

// GetReplicationAddressByIP returns the replication address using pod IP for reliable connectivity
func (pi *PodInfo) GetReplicationAddressByIP() string {
	if pi.Pod != nil && pi.Pod.Status.PodIP != "" {
		return fmt.Sprintf("%s:10000", pi.Pod.Status.PodIP)
	}
	return "" // Pod IP not available
}

// IsReadyForReplication checks if pod is ready for replication (has IP and passes readiness checks)
func (pi *PodInfo) IsReadyForReplication() bool {
	if pi.Pod == nil || pi.Pod.Status.PodIP == "" {
		return false
	}
	
	// Check Kubernetes readiness conditions
	for _, condition := range pi.Pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	
	return false
}

// ShouldBecomeMaster determines if this pod should be promoted to master
func (pi *PodInfo) ShouldBecomeMaster(currentMasterName string) bool {
	// Pod should become master if:
	// 1. It's currently selected as the master pod (by timestamp)
	// 2. AND it's not already in MASTER state
	return pi.Name == currentMasterName && pi.State != MASTER
}

// ShouldBecomeReplica determines if this pod should be demoted to replica
func (pi *PodInfo) ShouldBecomeReplica(currentMasterName string) bool {
	// Pod should become replica if:
	// 1. It's NOT the selected master pod
	// 2. AND it's not already in REPLICA state
	return pi.Name != currentMasterName && pi.State != REPLICA
}

// NeedsReplicationConfiguration determines if this pod needs replication changes
func (pi *PodInfo) NeedsReplicationConfiguration(currentMasterName string) bool {
	return pi.ShouldBecomeMaster(currentMasterName) || pi.ShouldBecomeReplica(currentMasterName)
}

// ClassifyClusterState determines the cluster state type based on pod roles
func (cs *ClusterState) ClassifyClusterState() ClusterStateType {
	var mainPods []string
	var replicaPods []string
	
	// Categorize pods by their actual Memgraph roles
	for podName, podInfo := range cs.Pods {
		switch podInfo.MemgraphRole {
		case "main":
			mainPods = append(mainPods, podName)
		case "replica":
			replicaPods = append(replicaPods, podName)
		}
	}
	
	// Classify based on main/replica distribution
	switch {
	case len(mainPods) == 0 && len(replicaPods) == 0:
		// No pods with role information yet
		return INITIAL_STATE
		
	case len(mainPods) == len(cs.Pods) && len(replicaPods) == 0:
		// All pods are main - fresh cluster
		return INITIAL_STATE
		
	case len(mainPods) == 1 && len(replicaPods) >= 0:
		// Exactly one main - healthy operational state
		return OPERATIONAL_STATE
		
	case len(mainPods) > 1:
		// Multiple main pods - split brain
		return SPLIT_BRAIN_STATE
		
	case len(mainPods) == 0 && len(replicaPods) > 0:
		// All replicas, no main - needs promotion
		return NO_MASTER_STATE
		
	case len(mainPods) > 0 && len(replicaPods) > 0:
		// Mixed state - some main, some replica, need to analyze
		if len(mainPods) == 1 {
			return OPERATIONAL_STATE
		} else {
			return MIXED_STATE
		}
		
	default:
		return MIXED_STATE
	}
}

// IsBootstrapSafe determines if it's safe to proceed during bootstrap
func (cs *ClusterState) IsBootstrapSafe() bool {
	stateType := cs.ClassifyClusterState()
	
	switch stateType {
	case INITIAL_STATE, OPERATIONAL_STATE:
		return true
	case MIXED_STATE, NO_MASTER_STATE, SPLIT_BRAIN_STATE:
		return false
	default:
		return false
	}
}

// GetMainPods returns list of pods with "main" role
func (cs *ClusterState) GetMainPods() []string {
	var mainPods []string
	for podName, podInfo := range cs.Pods {
		if podInfo.MemgraphRole == "main" {
			mainPods = append(mainPods, podName)
		}
	}
	return mainPods
}

// GetReplicaPods returns list of pods with "replica" role
func (cs *ClusterState) GetReplicaPods() []string {
	var replicaPods []string
	for podName, podInfo := range cs.Pods {
		if podInfo.MemgraphRole == "replica" {
			replicaPods = append(replicaPods, podName)
		}
	}
	return replicaPods
}

// DetermineMasterIndex determines which pod index (0 or 1) should be master
func (cs *ClusterState) DetermineMasterIndex(config *Config) (int, error) {
	mainPods := cs.GetMainPods()
	replicaPods := cs.GetReplicaPods()
	
	// Rule 1: If ALL pods are masters (fresh cluster scenario)
	if len(replicaPods) == 0 && len(mainPods) == len(cs.Pods) {
		// Fresh cluster - always choose index 0 as master
		log.Printf("Fresh cluster detected - selecting pod-0 as master")
		return 0, nil
	}
	
	// Rule 2: If ANY pod is in replica state (existing cluster)
	if len(replicaPods) > 0 {
		return cs.analyzeExistingCluster(mainPods, replicaPods, config)
	}
	
	// Rule 3: No pods have role information yet
	if len(mainPods) == 0 && len(replicaPods) == 0 {
		// Default to pod-0 when no role information available
		log.Printf("No role information available - defaulting to pod-0 as master")
		return 0, nil
	}
	
	// Fallback: select pod-0
	return 0, nil
}

// analyzeExistingCluster determines master index for existing clusters
func (cs *ClusterState) analyzeExistingCluster(mainPods, replicaPods []string, config *Config) (int, error) {
	// Look for existing master among eligible pods (pod-0 or pod-1)
	pod0Name := config.GetPodName(0)
	pod1Name := config.GetPodName(1)
	
	// Check if pod-0 is the current master
	for _, masterPod := range mainPods {
		if masterPod == pod0Name {
			log.Printf("Found existing master pod-0: %s", masterPod)
			return 0, nil
		}
	}
	
	// Check if pod-1 is the current master
	for _, masterPod := range mainPods {
		if masterPod == pod1Name {
			log.Printf("Found existing master pod-1: %s", masterPod)
			return 1, nil
		}
	}
	
	// Master is not pod-0 or pod-1 (unusual but possible)
	// Apply lower-index precedence rule
	log.Printf("Current master not in eligible pods (pod-0/pod-1)")
	log.Printf("Current masters: %v", mainPods)
	log.Printf("Applying lower-index precedence rule")
	
	// Check if pod-0 exists and is available
	if _, exists := cs.Pods[pod0Name]; exists {
		log.Printf("Selecting pod-0 as master (lower index precedence)")
		return 0, nil
	}
	
	// Check if pod-1 exists and is available
	if _, exists := cs.Pods[pod1Name]; exists {
		log.Printf("Selecting pod-1 as master (pod-0 not available)")
		return 1, nil
	}
	
	return -1, fmt.Errorf("neither pod-0 nor pod-1 available for master role")
}

// ValidateControllerState validates the internal controller state consistency
func (cs *ClusterState) ValidateControllerState() []string {
	var warnings []string
	
	// Validate target master index
	if cs.TargetMasterIndex < 0 || cs.TargetMasterIndex > 1 {
		warnings = append(warnings, fmt.Sprintf("Invalid target master index: %d (should be 0 or 1)", cs.TargetMasterIndex))
	}
	
	// Validate current master exists in pods
	if cs.CurrentMaster != "" {
		if _, exists := cs.Pods[cs.CurrentMaster]; !exists {
			warnings = append(warnings, fmt.Sprintf("Current master '%s' not found in discovered pods", cs.CurrentMaster))
		}
	}
	
	// Validate state type consistency
	actualStateType := cs.ClassifyClusterState()
	if cs.StateType != actualStateType {
		warnings = append(warnings, fmt.Sprintf("State type mismatch: recorded=%s, actual=%s", cs.StateType.String(), actualStateType.String()))
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

// MasterSelectionMetrics tracks master selection decision making
type MasterSelectionMetrics struct {
	Timestamp                time.Time
	StateType               ClusterStateType
	TargetMasterIndex       int
	SelectedMaster          string
	SelectionReason         string
	HealthyPodsCount        int
	SyncReplicaAvailable    bool
	FailoverDetected        bool
	DecisionFactors         []string
}

// LogMasterSelectionDecision logs detailed master selection metrics
func (cs *ClusterState) LogMasterSelectionDecision(metrics *MasterSelectionMetrics) {
	log.Printf("📊 MASTER SELECTION METRICS:")
	log.Printf("  Timestamp: %s", metrics.Timestamp.Format(time.RFC3339))
	log.Printf("  State Type: %s", metrics.StateType.String())
	log.Printf("  Target Master Index: %d", metrics.TargetMasterIndex)
	log.Printf("  Selected Master: %s", metrics.SelectedMaster)
	log.Printf("  Selection Reason: %s", metrics.SelectionReason)
	log.Printf("  Healthy Pods: %d", metrics.HealthyPodsCount)
	log.Printf("  SYNC Replica Available: %t", metrics.SyncReplicaAvailable)
	log.Printf("  Failover Detected: %t", metrics.FailoverDetected)
	log.Printf("  Decision Factors: %v", metrics.DecisionFactors)
}

// GetClusterHealthSummary returns a summary of cluster health
func (cs *ClusterState) GetClusterHealthSummary() map[string]interface{} {
	healthyPods := 0
	totalPods := len(cs.Pods)
	syncReplicaCount := 0
	mainPods := 0
	replicaPods := 0
	
	for _, podInfo := range cs.Pods {
		if podInfo.BoltAddress != "" && podInfo.MemgraphRole != "" {
			healthyPods++
		}
		
		if podInfo.IsSyncReplica {
			syncReplicaCount++
		}
		
		switch podInfo.MemgraphRole {
		case "main":
			mainPods++
		case "replica":
			replicaPods++
		}
	}
	
	return map[string]interface{}{
		"total_pods":         totalPods,
		"healthy_pods":       healthyPods,
		"unhealthy_pods":     totalPods - healthyPods,
		"main_pods":         mainPods,
		"replica_pods":      replicaPods,
		"sync_replicas":     syncReplicaCount,
		"current_master":    cs.CurrentMaster,
		"target_index":      cs.TargetMasterIndex,
		"state_type":        cs.StateType.String(),
		"bootstrap_phase":   cs.IsBootstrapPhase,
		"last_change":       cs.LastStateChange,
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