package controller

import (
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