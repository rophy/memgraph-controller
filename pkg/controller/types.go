package controller

import (
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
)

type PodState int

const (
	INITIAL PodState = iota // No role label, Memgraph is MAIN with no replicas
	MASTER                  // role=master label, Memgraph role is MAIN
	REPLICA                 // role=replica label, Memgraph role is REPLICA
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
	State                 PodState      // Derived from K8s labels + Memgraph queries
	Timestamp             time.Time     // Pod creation/restart time
	KubernetesRole        string        // Value of "role" label (empty, "master", "replica")
	MemgraphRole          string        // Result of SHOW REPLICATION ROLE ("MAIN", "REPLICA")
	BoltAddress           string        // Pod IP:7687 for Bolt connections
	ReplicationAddress    string        // <pod-name>.<service-name>:10000 for replication
	ReplicaName           string        // Pod name with dashes → underscores for REGISTER REPLICA
	Replicas              []string      // Result of SHOW REPLICAS (only for MAIN nodes)
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
	
	// Get Kubernetes role label
	kubernetesRole := ""
	if pod.Labels != nil {
		kubernetesRole = pod.Labels["role"]
	}

	return &PodInfo{
		Name:               podName,
		State:              INITIAL, // Will be determined later
		Timestamp:          timestamp,
		KubernetesRole:     kubernetesRole,
		MemgraphRole:       "",      // Will be queried later
		BoltAddress:        boltAddress,
		ReplicationAddress: replicationAddress,
		ReplicaName:        replicaName,
		Replicas:           []string{},
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

// ClassifyPodState determines the actual pod state based on Kubernetes labels and Memgraph role
func (pi *PodInfo) ClassifyState() PodState {
	// If we don't have Memgraph role information yet, return current state
	if pi.MemgraphRole == "" {
		return pi.State
	}

	// State classification rules from implementation plan:
	// - INITIAL: No role label AND Memgraph role is MAIN with no replicas
	// - MASTER: role=master label AND Memgraph role is MAIN
	// - REPLICA: role=replica label AND Memgraph role is REPLICA

	switch {
	case pi.KubernetesRole == "" && pi.MemgraphRole == "MAIN" && len(pi.Replicas) == 0:
		return INITIAL
	case pi.KubernetesRole == "master" && pi.MemgraphRole == "MAIN":
		return MASTER
	case pi.KubernetesRole == "replica" && pi.MemgraphRole == "REPLICA":
		return REPLICA
	default:
		// State inconsistency detected - return current state but log the issue
		return pi.State
	}
}

// DetectStateInconsistency checks if Kubernetes labels match Memgraph reality
func (pi *PodInfo) DetectStateInconsistency() *StateInconsistency {
	if pi.MemgraphRole == "" {
		// Can't detect inconsistency without Memgraph role info
		return nil
	}

	expectedState := pi.ClassifyState()
	
	// Check for actual inconsistencies between K8s labels and Memgraph state
	hasInconsistency := false
	
	// Check specific inconsistency patterns
	switch {
	case pi.KubernetesRole == "" && pi.MemgraphRole == "MAIN" && len(pi.Replicas) > 0:
		hasInconsistency = true
	case pi.KubernetesRole == "master" && pi.MemgraphRole == "REPLICA":
		hasInconsistency = true
	case pi.KubernetesRole == "replica" && pi.MemgraphRole == "MAIN":
		hasInconsistency = true
	case pi.KubernetesRole == "" && pi.MemgraphRole == "REPLICA":
		hasInconsistency = true
	}

	if !hasInconsistency {
		return nil // No inconsistency
	}

	return &StateInconsistency{
		PodName:           pi.Name,
		KubernetesRole:    pi.KubernetesRole,
		MemgraphRole:      pi.MemgraphRole,
		CurrentState:      pi.State,
		ExpectedState:     expectedState,
		ReplicaCount:      len(pi.Replicas),
		Description:       buildInconsistencyDescription(pi),
	}
}

type StateInconsistency struct {
	PodName        string
	KubernetesRole string
	MemgraphRole   string
	CurrentState   PodState
	ExpectedState  PodState
	ReplicaCount   int
	Description    string
}

func buildInconsistencyDescription(pi *PodInfo) string {
	switch {
	case pi.KubernetesRole == "" && pi.MemgraphRole == "MAIN" && len(pi.Replicas) > 0:
		return fmt.Sprintf("Pod has no role label but is MAIN with %d replicas (should be MASTER)", len(pi.Replicas))
	case pi.KubernetesRole == "master" && pi.MemgraphRole == "REPLICA":
		return "Pod labeled as master but Memgraph role is REPLICA"
	case pi.KubernetesRole == "replica" && pi.MemgraphRole == "MAIN":
		return "Pod labeled as replica but Memgraph role is MAIN"
	case pi.KubernetesRole == "" && pi.MemgraphRole == "REPLICA":
		return "Pod has no role label but Memgraph role is REPLICA"
	case pi.KubernetesRole == "master" && pi.MemgraphRole == "MAIN" && len(pi.Replicas) == 0:
		return "Pod labeled as master but has no replicas (might be INITIAL state)"
	case pi.KubernetesRole == "replica" && pi.MemgraphRole == "REPLICA":
		return "Pod correctly configured as replica"
	default:
		return fmt.Sprintf("Unknown inconsistency: k8s_role=%s, memgraph_role=%s, replicas=%d", 
			pi.KubernetesRole, pi.MemgraphRole, len(pi.Replicas))
	}
}