package controller

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"memgraph-controller/internal/common"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// LeaderElection handles Kubernetes leader election for HA controller
type LeaderElection struct {
	clientset       kubernetes.Interface
	config          *common.Config
	identity        string
	currentLeader   string
	currentLeaderMu sync.RWMutex
}

// NewLeaderElection creates a new leader election instance
func NewLeaderElection(clientset kubernetes.Interface, config *common.Config) (*LeaderElection, error) {
	identity, err := getPodIdentity()
	if err != nil {
		return nil, fmt.Errorf("failed to get pod identity: %w", err)
	}
	return &LeaderElection{
		clientset:       clientset,
		config:          config,
		identity:        identity,
		currentLeader:   "",
		currentLeaderMu: sync.RWMutex{},
	}, nil
}

// startLeaderElection starts the leader election process (in background)
func (c *MemgraphController) startLeaderElection(ctx context.Context) {
	go func() {
		ctx, logger := common.WithAttr(ctx, "thread", "leaderElection")

		le := c.leaderElection
		logger.Info("starting leader election with identity", "identity", le.identity)

		// Create resource lock for leader election
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{
				Name:      "memgraph-controller-leader",
				Namespace: le.config.Namespace,
			},
			Client: le.clientset.CoordinationV1(),
			LockConfig: resourcelock.ResourceLockConfig{
				Identity: le.identity,
			},
		}

		// Create leader election configuration
		leaderElectionConfig := leaderelection.LeaderElectionConfig{
			Lock:            lock,
			LeaseDuration:   15 * time.Second,
			RenewDeadline:   10 * time.Second,
			RetryPeriod:     2 * time.Second,
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: c.onStartLeading,
				OnStoppedLeading: c.onStopLeading,
				OnNewLeader:      le.onNewLeader,
			},
		}

		// Start leader election (blocking)
		leaderelection.RunOrDie(ctx, leaderElectionConfig)
	}()
}

// onStartLeading is the callback function for when the leader election starts
func (c *MemgraphController) onStartLeading(ctx context.Context) {
	le := c.leaderElection
	logger := common.GetLoggerFromContext(ctx)
	le.currentLeaderMu.Lock()
	newLeader := le.currentLeader != le.identity
	le.currentLeader = le.identity
	le.currentLeaderMu.Unlock()

	// Log if this is an actual leader change
	if newLeader {
		logger.Info("🎯 started leading as", "identity", c.leaderElection.identity)
	}

	// Record leadership metrics
	if c.promMetrics != nil {
		c.promMetrics.UpdateLeadershipStatus(true)
		if newLeader {
			c.promMetrics.RecordLeadershipChange()
		}
		c.promMetrics.RecordElection()
	}
}

// onStopLeading is the callback function for when the leader election stops
func (c *MemgraphController) onStopLeading() {
	le := c.leaderElection
	logger := common.GetLogger()
	le.currentLeaderMu.Lock()
	le.currentLeader = ""
	le.currentLeaderMu.Unlock()

	logger.Info("⏹️  Lost leadership - stopping operations", "identity", le.identity)
}

// onNewLeader is the callback function for when the leader election changes
func (le *LeaderElection) onNewLeader(currentLeader string) {
	logger := common.GetLogger()

	// Only log if this is an actual leader change
	if currentLeader != le.currentLeader {
		logger.Info("👑 New leader elected", "last_known_leader", le.currentLeader, "new_leader", currentLeader)
		le.currentLeaderMu.Lock()
		le.currentLeader = currentLeader
		le.currentLeaderMu.Unlock()
	}
}

// IsLeader returns whether this controller instance is the current leader
func (le *LeaderElection) IsLeader() bool {
	return le.currentLeader == le.identity
}

// getPodIdentity returns the identity for this controller instance
func getPodIdentity() (string, error) {
	// Try to get pod name from environment (set by Kubernetes)
	podName := os.Getenv("POD_NAME")
	if podName != "" {
		return podName, nil
	}

	// Try to get hostname as fallback
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("failed to get hostname: %w", err)
	}

	common.GetLogger().Warn("POD_NAME env var not set, using hostname", "hostname", hostname)
	return hostname, nil
}

// GetCurrentLeader returns the identity of the current leader by querying the lease
func (le *LeaderElection) GetCurrentLeader(ctx context.Context) (string, error) {
	lease, err := le.clientset.CoordinationV1().Leases(le.config.Namespace).Get(
		ctx,
		"memgraph-controller-leader",
		metav1.GetOptions{},
	)
	if err != nil {
		return "", fmt.Errorf("failed to get leader lease: %w", err)
	}

	if lease.Spec.HolderIdentity == nil {
		return "", fmt.Errorf("no current leader")
	}

	return *lease.Spec.HolderIdentity, nil
}

// GetMyIdentity returns the identity of this controller instance
func (le *LeaderElection) GetMyIdentity() string {
	return le.identity
}
