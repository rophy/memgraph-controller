package controller

import (
	"time"
)

type PodState int

const (
	INITIAL PodState = iota // Memgraph is MAIN with no replicas
	MAIN                    // Memgraph role is MAIN with replicas
	REPLICA                 // Memgraph role is REPLICA
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



type StateInconsistency struct {
	PodName       string
	MemgraphRole  string
	CurrentState  PodState
	ExpectedState PodState
	ReplicaCount  int
	Description   string
}


// MainSelectionMetrics tracks main selection decision making
type MainSelectionMetrics struct {
	Timestamp time.Time
	// TargetMainIndex removed - now accessed via controller.getTargetMainIndex()
	SelectedMain         string
	SelectionReason      string
	HealthyPodsCount     int
	SyncReplicaAvailable bool
	FailoverDetected     bool
	DecisionFactors      []string
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

