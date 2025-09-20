package httpapi

import (
	"context"
	"time"
)

// ControllerInterface defines the methods needed by the HTTP API from the controller
type ControllerInterface interface {
	GetClusterStatus(ctx context.Context) (*StatusResponse, error)
	GetLeaderElection() LeaderElectionInterface
	IsRunning() bool
	ResetAllConnections(ctx context.Context) (int, error)
}

// LeaderElectionInterface defines the methods needed from leader election
type LeaderElectionInterface interface {
	GetCurrentLeader(ctx context.Context) (string, error)
	GetMyIdentity() string
	IsLeader() bool
}

// ReconciliationMetrics represents controller reconciliation metrics
type ReconciliationMetrics struct {
	TotalReconciliations      int64         `json:"total_reconciliations"`
	SuccessfulReconciliations int64         `json:"successful_reconciliations"`
	FailedReconciliations     int64         `json:"failed_reconciliations"`
	AverageReconciliationTime time.Duration `json:"average_reconciliation_time"`
	LastReconciliationTime    time.Time     `json:"last_reconciliation_time"`
	LastReconciliationReason  string        `json:"last_reconciliation_reason"`
	LastReconciliationError   string        `json:"last_reconciliation_error,omitempty"`
}
