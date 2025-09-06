package logging

import (
	"context"
	"log/slog"
)

// Event types for structured logging
const (
	EventPodCreated         = "pod_created"
	EventPodDeleted         = "pod_deleted"
	EventPodReady           = "pod_ready"
	EventPodNotReady        = "pod_not_ready"
	EventReconcileStarted   = "reconcile_started"
	EventReconcileCompleted = "reconcile_completed"
	EventReconcileFailed    = "reconcile_failed"
	EventFailoverStarted    = "failover_started"
	EventFailoverCompleted  = "failover_completed"
	EventFailoverFailed     = "failover_failed"
	EventConnectionOpen     = "connection_open"
	EventConnectionClosed   = "connection_closed"
	EventLeaderChanged      = "leader_changed"
	EventHealthCheck        = "health_check"
	EventReplicationSync    = "replication_sync"
)

// EventLogger provides methods for logging domain-specific events
type EventLogger struct {
	logger *Logger
}

// NewEventLogger creates a new event logger
func NewEventLogger(logger *Logger) *EventLogger {
	return &EventLogger{logger: logger}
}

// LogPodEvent logs pod-related events
func (e *EventLogger) LogPodEvent(ctx context.Context, event string, podName string, fields map[string]any) {
	args := []any{
		"event", event,
		"pod", podName,
	}
	
	for k, v := range fields {
		args = append(args, k, v)
	}
	
	e.logger.WithContext(ctx).Info("Pod event", args...)
}

// LogReconcileEvent logs reconciliation events
func (e *EventLogger) LogReconcileEvent(ctx context.Context, event string, duration float64, fields map[string]any) {
	args := []any{
		"event", event,
		"duration_ms", duration,
	}
	
	for k, v := range fields {
		args = append(args, k, v)
	}
	
	level := slog.LevelInfo
	if event == EventReconcileFailed {
		level = slog.LevelError
	}
	
	e.logger.WithContext(ctx).Log(ctx, level, "Reconcile event", args...)
}

// LogFailoverEvent logs failover events
func (e *EventLogger) LogFailoverEvent(ctx context.Context, event string, oldMain, newMain string, fields map[string]any) {
	args := []any{
		"event", event,
		"old_main", oldMain,
		"new_main", newMain,
	}
	
	for k, v := range fields {
		args = append(args, k, v)
	}
	
	level := slog.LevelWarn
	if event == EventFailoverFailed {
		level = slog.LevelError
	}
	
	e.logger.WithContext(ctx).Log(ctx, level, "Failover event", args...)
}

// LogConnectionEvent logs connection events
func (e *EventLogger) LogConnectionEvent(ctx context.Context, event string, clientAddr, backendAddr string, fields map[string]any) {
	args := []any{
		"event", event,
		"client_addr", clientAddr,
		"backend_addr", backendAddr,
	}
	
	for k, v := range fields {
		args = append(args, k, v)
	}
	
	e.logger.WithContext(ctx).Info("Connection event", args...)
}

// LogHealthCheck logs health check events
func (e *EventLogger) LogHealthCheck(ctx context.Context, podName string, healthy bool, details map[string]any) {
	args := []any{
		"event", EventHealthCheck,
		"pod", podName,
		"healthy", healthy,
	}
	
	for k, v := range details {
		args = append(args, k, v)
	}
	
	level := slog.LevelInfo
	if !healthy {
		level = slog.LevelWarn
	}
	
	e.logger.WithContext(ctx).Log(ctx, level, "Health check", args...)
}

// LogLeaderElection logs leader election events
func (e *EventLogger) LogLeaderElection(ctx context.Context, oldLeader, newLeader string, fields map[string]any) {
	args := []any{
		"event", EventLeaderChanged,
		"old_leader", oldLeader,
		"new_leader", newLeader,
	}
	
	for k, v := range fields {
		args = append(args, k, v)
	}
	
	e.logger.WithContext(ctx).Info("Leader election", args...)
}

// LogReplicationSync logs replication synchronization events
func (e *EventLogger) LogReplicationSync(ctx context.Context, mainPod string, replicas []string, success bool, fields map[string]any) {
	args := []any{
		"event", EventReplicationSync,
		"main_pod", mainPod,
		"replicas", replicas,
		"success", success,
	}
	
	for k, v := range fields {
		args = append(args, k, v)
	}
	
	level := slog.LevelInfo
	if !success {
		level = slog.LevelError
	}
	
	e.logger.WithContext(ctx).Log(ctx, level, "Replication sync", args...)
}