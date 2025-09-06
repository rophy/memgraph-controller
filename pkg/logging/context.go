package logging

import "context"

// ContextKey represents keys for context values
type ContextKey string

const (
	RequestIDKey  ContextKey = "request_id"
	TraceIDKey    ContextKey = "trace_id"
	OperationKey  ContextKey = "operation"
	PodKey        ContextKey = "pod"
	ClusterKey    ContextKey = "cluster"
)

// WithRequestID adds a request ID to the context
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, RequestIDKey, requestID)
}

// WithTraceID adds a trace ID to the context
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, TraceIDKey, traceID)
}

// WithOperation adds an operation name to the context
func WithOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, OperationKey, operation)
}

// WithPod adds a pod name to the context
func WithPod(ctx context.Context, podName string) context.Context {
	return context.WithValue(ctx, PodKey, podName)
}

// WithCluster adds a cluster name to the context
func WithCluster(ctx context.Context, clusterName string) context.Context {
	return context.WithValue(ctx, ClusterKey, clusterName)
}