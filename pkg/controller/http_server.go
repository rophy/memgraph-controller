package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// HTTPServer manages the status API HTTP server
type HTTPServer struct {
	controller *MemgraphController
	server     *http.Server
	config     *Config
}

// NewHTTPServer creates a new HTTP server for status API
func NewHTTPServer(controller *MemgraphController, config *Config) *HTTPServer {
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
	mux.HandleFunc("/", httpServer.handleRoot)

	return httpServer
}

// Start begins listening for HTTP requests (non-blocking)
func (h *HTTPServer) Start() error {
	log.Printf("Starting HTTP server on port %s", h.config.HTTPPort)

	go func() {
		if err := h.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Give server a moment to start up
	time.Sleep(100 * time.Millisecond)
	log.Printf("HTTP server started successfully on port %s", h.config.HTTPPort)
	return nil
}

// Stop gracefully shuts down the HTTP server
func (h *HTTPServer) Stop(ctx context.Context) error {
	log.Println("Shutting down HTTP server...")

	err := h.server.Shutdown(ctx)
	if err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
		return err
	}

	log.Println("HTTP server stopped successfully")
	return nil
}

// handleStatus handles GET /api/v1/status requests
func (h *HTTPServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Println("Handling status API request")

	// Create context with timeout for status collection
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// Collect cluster status
	status, err := h.controller.GetClusterStatus(ctx)
	if err != nil {
		log.Printf("Failed to get cluster status: %v", err)
		http.Error(w, fmt.Sprintf("Failed to get cluster status: %v", err), http.StatusInternalServerError)
		return
	}

	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	// Encode and send response
	if err := json.NewEncoder(w).Encode(status); err != nil {
		log.Printf("Failed to encode status response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	log.Printf("Successfully handled status API request: %d pods, main: %s",
		status.ClusterState.TotalPods, status.ClusterState.CurrentMain)
}

// handleLeadership handles GET /api/v1/leadership requests
func (h *HTTPServer) handleLeadership(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Println("Handling leadership API request")

	// Create context with timeout
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Get current leader from Kubernetes lease
	leaderElection := h.controller.GetLeaderElection()
	currentLeader, err := leaderElection.GetCurrentLeader(ctx)
	if err != nil {
		log.Printf("Failed to get current leader: %v", err)
		http.Error(w, fmt.Sprintf("Failed to get current leader: %v", err), http.StatusInternalServerError)
		return
	}

	// Get this pod's identity
	myIdentity, err := leaderElection.GetMyIdentity()
	if err != nil {
		log.Printf("Failed to get my identity: %v", err)
		http.Error(w, fmt.Sprintf("Failed to get pod identity: %v", err), http.StatusInternalServerError)
		return
	}

	// Check if this pod is the leader
	isLeader := h.controller.IsLeader()

	// Prepare response
	response := map[string]interface{}{
		"current_leader":    currentLeader,
		"my_identity":       myIdentity,
		"i_am_leader":       isLeader,
		"leader_match":      currentLeader == myIdentity,
		"timestamp":         time.Now(),
		"lease_name":        "memgraph-controller-leader",
		"namespace":         h.config.Namespace,
	}

	// Set response headers
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	// Encode and send response
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("Failed to encode leadership response: %v", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	log.Printf("Leadership info: leader=%s, me=%s, i_am_leader=%t", currentLeader, myIdentity, isLeader)
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
	if !h.controller.IsLeader() {
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
