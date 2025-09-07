package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"memgraph-controller/pkg/common"
)

// LeaderElection handles Kubernetes leader election for HA controller
type LeaderElection struct {
	clientset        kubernetes.Interface
	config           *common.Config
	onStartedLeading func(ctx context.Context)
	onStoppedLeading func()
	onNewLeader      func(identity string)
}

// NewLeaderElection creates a new leader election instance
func NewLeaderElection(clientset kubernetes.Interface, config *common.Config) *LeaderElection {
	return &LeaderElection{
		clientset: clientset,
		config:    config,
	}
}

// SetCallbacks sets the callback functions for leader election events
func (le *LeaderElection) SetCallbacks(
	onStartedLeading func(ctx context.Context),
	onStoppedLeading func(),
	onNewLeader func(identity string),
) {
	le.onStartedLeading = onStartedLeading
	le.onStoppedLeading = onStoppedLeading
	le.onNewLeader = onNewLeader
}

// Run starts the leader election process (blocking)
func (le *LeaderElection) Run(ctx context.Context) error {
	// Get pod identity for leader election
	identity, err := le.getPodIdentity()
	if err != nil {
		return fmt.Errorf("failed to get pod identity: %w", err)
	}

	logger.Info("starting leader election with identity", "identity", identity)

	// Create resource lock for leader election
	lock, err := le.createResourceLock(identity)
	if err != nil {
		return fmt.Errorf("failed to create resource lock: %w", err)
	}

	// Create leader election configuration
	leaderElectionConfig := leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logger.Info("started leading as", "identity", identity)
				if le.onStartedLeading != nil {
					le.onStartedLeading(ctx)
				}
			},
			OnStoppedLeading: func() {
				logger.Info("stopped leading as", "identity", identity)
				if le.onStoppedLeading != nil {
					le.onStoppedLeading()
				}
			},
			OnNewLeader: func(currentLeader string) {
				if currentLeader != identity {
					logger.Info("new leader elected", "current_leader", currentLeader, "identity", identity)
				}
				if le.onNewLeader != nil {
					le.onNewLeader(currentLeader)
				}
			},
		},
	}

	// Start leader election (blocking)
	leaderelection.RunOrDie(ctx, leaderElectionConfig)
	return nil
}

// getPodIdentity returns the identity for this controller instance
func (le *LeaderElection) getPodIdentity() (string, error) {
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

	logger.Warn("POD_NAME env var not set, using hostname", "hostname", hostname)
	return hostname, nil
}

// createResourceLock creates a resource lock for leader election
func (le *LeaderElection) createResourceLock(identity string) (resourcelock.Interface, error) {
	// Use Lease lock (recommended for Kubernetes 1.14+)
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "memgraph-controller-leader",
			Namespace: le.config.Namespace,
		},
		Client: le.clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	return lock, nil
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
func (le *LeaderElection) GetMyIdentity() (string, error) {
	return le.getPodIdentity()
}
