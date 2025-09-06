// Package logging provides structured logging using Go's slog package with JSONL output.
//
// Basic Usage:
//
//	logger := logging.NewControllerLogger(logging.GetLogLevel())
//	logger.Info("Pod ready", "pod", podName, "cluster", clusterName)
//
// Context-aware logging:
//
//	ctx = logging.WithPod(ctx, "memgraph-0")
//	ctx = logging.WithOperation(ctx, "reconcile")
//	contextLogger := logger.WithContext(ctx)
//	contextLogger.Info("Processing pod")
//
// Structured fields:
//
//	logger.WithFields(map[string]any{
//		"pod":     podName,
//		"status":  "ready",
//		"uptime":  duration,
//	}).Info("Pod status update")
//
// Error logging:
//
//	logger.Error("Failed to connect", "error", err, "pod", podName)
//
// All logs are output in JSONL format for structured parsing and analysis.
package logging