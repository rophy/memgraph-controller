package controller

import (
	"time"
)

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
