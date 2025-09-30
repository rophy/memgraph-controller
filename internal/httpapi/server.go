package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"memgraph-controller/internal/common"
	"memgraph-controller/internal/metrics"
)

// HTTPServer manages the status API HTTP server
type HTTPServer struct {
	controller ControllerInterface
	server     *http.Server
	config     *common.Config
}

// NewHTTPServer creates a new HTTP server for status API
func NewHTTPServer(controller ControllerInterface, config *common.Config) *HTTPServer {
	mux := http.NewServeMux()

	httpServer := &HTTPServer{
		controller: controller,
		config:     config,
		server: &http.Server{
			Addr:         fmt.Sprintf(":%s", config.HTTPPort),
			Handler:      mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
	}

	// Register routes
	mux.HandleFunc("/api/v1/status", httpServer.handleStatus)
	mux.HandleFunc("/api/v1/leadership", httpServer.handleLeadership)
	mux.HandleFunc("/health", httpServer.handleHealth)
	mux.HandleFunc("/livez", httpServer.handleLiveness)
	mux.HandleFunc("/readyz", httpServer.handleReadiness)
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/", httpServer.handleRoot)

	mux.HandleFunc("/prestop-hook/{pod_name}", httpServer.handlePreStopHook)

	// Admin endpoints (only enabled if ENABLE_ADMIN_API=true)
	if os.Getenv("ENABLE_ADMIN_API") == "true" {
		mux.HandleFunc("/api/v1/admin/reset-connections", httpServer.handleResetConnections)
	}

	return httpServer
}

// Start begins listening for HTTP requests (non-blocking)
func (h *HTTPServer) Start(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
	logger.Info("Starting HTTP server", "port", h.config.HTTPPort)

	go func() {
		if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Info("HTTP server error", "error", err)
		}
	}()

	// Give server a moment to start up
	time.Sleep(100 * time.Millisecond)
	logger.Info("HTTP server started successfully", "port", h.config.HTTPPort)
}

// Stop gracefully shuts down the HTTP server
func (h *HTTPServer) Stop(ctx context.Context) error {
	logger := common.GetLogger()
	logger.Info("Shutting down HTTP server...")

	err := h.server.Shutdown(ctx)
	if err != nil {
		logger.Info("HTTP server shutdown error", "error", err)
		return err
	}

	logger.Info("HTTP server stopped successfully")
	return nil
}

// handleStatus handles GET /api/v1/status requests
func (h *HTTPServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	logger := common.GetLogger()
	logger.Info("Handling status API request")

	// Create context with timeout for status collection
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// Collect cluster status
	status, err := h.controller.GetClusterStatus(ctx)
	if err != nil {
		logger.Info("Failed to get cluster status", "error", err)
		http.Error(w, fmt.Sprintf("Failed to get cluster status: %v", err), http.StatusInternalServerError)
		return
	}

	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	// Encode and send response
	if err := json.NewEncoder(w).Encode(status); err != nil {
		logger.Info("Failed to encode status response", "error", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	logger.Info("Successfully handled status API request", "pod_count", status.ClusterState.TotalPods, "main_pod", status.ClusterState.CurrentMain)
}

// handleLeadership handles GET /api/v1/leadership requests
func (h *HTTPServer) handleLeadership(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	logger := common.GetLogger()
	logger.Info("Handling leadership API request")

	// Create context with timeout
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Get current leader from Kubernetes lease
	leaderElection := h.controller.GetLeaderElection()
	currentLeader, err := leaderElection.GetCurrentLeader(ctx)
	if err != nil {
		logger.Info("Failed to get current leader", "error", err)
		http.Error(w, fmt.Sprintf("Failed to get current leader: %v", err), http.StatusInternalServerError)
		return
	}

	// Get this pod's identity
	myIdentity := leaderElection.GetMyIdentity()

	// Check if this pod is the leader
	isLeader := h.controller.GetLeaderElection().IsLeader()

	// Prepare response
	response := map[string]interface{}{
		"current_leader": currentLeader,
		"my_identity":    myIdentity,
		"i_am_leader":    isLeader,
		"leader_match":   currentLeader == myIdentity,
		"timestamp":      time.Now(),
		"lease_name":     "memgraph-controller-leader",
		"namespace":      h.config.Namespace,
	}

	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	// Encode and send response
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Info("Failed to encode leadership response", "error", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	logger.Info("Leadership info", "leader", currentLeader, "me", myIdentity, "i_am_leader", isLeader)
}

// handleHealth handles GET /health requests for basic health checks
func (h *HTTPServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]interface{}{
		"status":    "ok",
		"timestamp": time.Now(),
		"service":   "memgraph-controller",
	}

	json.NewEncoder(w).Encode(response)
}

// handleRoot handles GET / requests with basic service information
func (h *HTTPServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	response := map[string]interface{}{
		"service": "memgraph-controller",
		"version": "1.0.0",
		"endpoints": []string{
			"/api/v1/status",
			"/api/v1/leadership",
			"/health",
			"/livez",
			"/readyz",
		},
		"timestamp": time.Now(),
	}

	json.NewEncoder(w).Encode(response)
}

// handleLiveness handles GET /livez requests for Kubernetes liveness probes
// Returns 200 OK if the controller process is running and healthy
func (h *HTTPServer) handleLiveness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Basic liveness check - controller is alive if HTTP server is responding
	// and controller is marked as running
	if !h.controller.IsRunning() {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("Controller not running"))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// handleReadiness handles GET /readyz requests for Kubernetes readiness probes
// Returns 200 OK only if this pod is the current leader (for leader-only gateway)
func (h *HTTPServer) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Check if this pod is the leader
	if !h.controller.GetLeaderElection().IsLeader() {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("Not leader"))
		return
	}

	// Additional readiness checks could be added here:
	// - Gateway server is running and healthy
	// - Can connect to Kubernetes API
	// - Can connect to Memgraph pods

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// handleResetConnections handles POST /api/v1/admin/reset-connections requests
func (h *HTTPServer) handleResetConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	logger := common.GetLogger()
	logger.Info("Admin API: Resetting all Memgraph connections")

	// Create context with timeout
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Reset connections
	closedCount, err := h.controller.ResetAllConnections(ctx)
	if err != nil {
		logger.Info("Failed to reset connections", "error", err)
		http.Error(w, fmt.Sprintf("Failed to reset connections: %v", err), http.StatusInternalServerError)
		return
	}

	// Prepare response
	response := map[string]interface{}{
		"status":            "success",
		"connections_reset": closedCount,
		"timestamp":         time.Now(),
		"message":           fmt.Sprintf("Successfully reset %d connections", closedCount),
	}

	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// Encode and send response
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Info("Failed to encode reset connections response", "error", err)
		return
	}

	logger.Info("Admin API: Successfully reset connections", "count", closedCount)
}

// handlePreStopHook handles POST /prestop-hook/{pod_name} requests
func (h *HTTPServer) handlePreStopHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Get timeout from environment variable, default to 600 seconds
	timeoutSeconds := 600
	if envTimeout := os.Getenv("PRESTOP_TIMEOUT_SECONDS"); envTimeout != "" {
		if parsed, err := strconv.Atoi(envTimeout); err == nil && parsed > 0 {
			timeoutSeconds = parsed
		}
	}

	// Wait for up to the configured seconds for the preStop hook to complete
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	ctx, logger := common.NewLoggerContext(ctx)

	// Protect the request with HTTP Basic Authentication
	// Using the same username / password as memgraph.
	username, password, ok := r.BasicAuth()
	if !ok || username != h.config.MemgraphUser || password != h.config.MemgraphPassword {
		logger.Warn("Unauthorized request", "username", username)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	podName := r.PathValue("pod_name")

	logger.Info("handlePreStopHook started", "pod_name", podName)
	// Clear gateway upstreams
	err := h.controller.HandlePreStopHook(ctx, podName)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to handle preStop hook: %v", err), http.StatusInternalServerError)
		logger.Info("handlePreStopHook failed", "error", err, "pod_name", podName)
	} else {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(""))
		logger.Info("handlePreStopHook completed", "pod_name", podName)
	}
}
