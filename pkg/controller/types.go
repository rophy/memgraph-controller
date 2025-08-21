package controller

import (
	"fmt"
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

type ClusterState struct {
	Pods          map[string]*PodInfo
	CurrentMaster string
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

// GetReplicationAddress returns the replication address for this pod
func (pi *PodInfo) GetReplicationAddress(serviceName string) string {
	return fmt.Sprintf("%s.%s:10000", pi.Name, serviceName)
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