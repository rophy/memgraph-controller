package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// MasterEndpointProvider is a function that returns the current master endpoint
type MasterEndpointProvider func(ctx context.Context) (string, error)

// Server represents the gateway server that proxies Bolt protocol connections
type Server struct {
	config              *Config
	listener            net.Listener
	connections         *ConnectionTracker
	masterProvider      MasterEndpointProvider
	
	// Current master endpoint
	currentMaster string
	masterMu      sync.RWMutex
	
	// Server state
	isRunning    int32  // atomic boolean
	shutdownCh   chan struct{}
	wg           sync.WaitGroup
	
	// Metrics
	totalConnections    int64
	activeConnections   int64
	rejectedConnections int64
	errors              int64
	failovers           int64
	
	// Health monitoring
	lastHealthCheck     time.Time
	healthStatus        string
	healthMu            sync.RWMutex
}

// NewServer creates a new gateway server with the given configuration
func NewServer(config *Config, masterProvider MasterEndpointProvider) *Server {
	return &Server{
		config:         config,
		connections:    NewConnectionTracker(config.MaxConnections),
		masterProvider: masterProvider,
		shutdownCh:     make(chan struct{}),
		healthStatus:   "unknown",
	}
}

// SetCurrentMaster updates the current master endpoint that connections should be proxied to
func (s *Server) SetCurrentMaster(endpoint string) {
	s.masterMu.Lock()
	defer s.masterMu.Unlock()
	
	if s.currentMaster != endpoint {
		oldMaster := s.currentMaster
		s.currentMaster = endpoint
		
		if oldMaster != "" {
			// This is a failover event, terminate all existing connections
			log.Printf("Gateway: Master failover detected %s -> %s, terminating all connections", oldMaster, endpoint)
			atomic.AddInt64(&s.failovers, 1)
			
			// Terminate all existing connections to force clients to reconnect to new master
			s.terminateAllConnections()
		} else {
			log.Printf("Gateway: Initial master endpoint set to %s", endpoint)
		}
	}
}

// terminateAllConnections terminates all active connections to force client reconnection
func (s *Server) terminateAllConnections() {
	activeConnections := s.connections.GetCount()
	if activeConnections == 0 {
		log.Println("Gateway: No active connections to terminate")
		return
	}
	
	log.Printf("Gateway: Terminating %d active connections due to master failover", activeConnections)
	
	// Get all active sessions and close them
	sessions := s.connections.GetAllSessions()
	terminatedCount := 0
	for _, session := range sessions {
		go func(s *ProxySession) {
			s.Close()
		}(session) // Close asynchronously to avoid blocking
		terminatedCount++
	}
	
	log.Printf("Gateway: Initiated termination of %d connections", terminatedCount)
	
	// Update health status to reflect failover state
	s.healthMu.Lock()
	s.healthStatus = "failover-in-progress"
	s.lastHealthCheck = time.Now()
	s.healthMu.Unlock()
}

// GetCurrentMaster returns the current master endpoint
func (s *Server) GetCurrentMaster() string {
	s.masterMu.RLock()
	defer s.masterMu.RUnlock()
	return s.currentMaster
}

// Start starts the gateway server and begins accepting connections
func (s *Server) Start(ctx context.Context) error {
	if !s.config.Enabled {
		log.Println("Gateway: Server disabled, skipping startup")
		return nil
	}
	
	if err := s.config.Validate(); err != nil {
		return fmt.Errorf("gateway configuration invalid: %w", err)
	}
	
	if !atomic.CompareAndSwapInt32(&s.isRunning, 0, 1) {
		return fmt.Errorf("gateway server is already running")
	}
	
	listener, err := net.Listen("tcp", s.config.BindAddress)
	if err != nil {
		atomic.StoreInt32(&s.isRunning, 0)
		return fmt.Errorf("failed to listen on %s: %w", s.config.BindAddress, err)
	}
	
	s.listener = listener
	log.Printf("Gateway: Server listening on %s", s.config.BindAddress)
	
	// Start accepting connections in a goroutine
	s.wg.Add(1)
	go s.acceptConnections()
	
	return nil
}

// Stop gracefully shuts down the gateway server
func (s *Server) Stop(ctx context.Context) error {
	if !s.config.Enabled {
		return nil // Nothing to stop if disabled
	}
	
	if !atomic.CompareAndSwapInt32(&s.isRunning, 1, 0) {
		return nil // Already stopped
	}
	
	log.Println("Gateway: Shutting down server...")
	
	// Signal shutdown
	close(s.shutdownCh)
	
	// Close listener to stop accepting new connections
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			log.Printf("Gateway: Error closing listener: %v", err)
		}
	}
	
	// Close all active connections
	s.connections.CloseAll()
	
	// Wait for accept goroutine to finish
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	
	// Wait for graceful shutdown or timeout
	select {
	case <-done:
		log.Println("Gateway: Server shutdown complete")
		return nil
	case <-ctx.Done():
		log.Println("Gateway: Shutdown timeout, forcing termination")
		return ctx.Err()
	}
}

// acceptConnections runs the main accept loop for incoming connections
func (s *Server) acceptConnections() {
	defer s.wg.Done()
	defer log.Println("Gateway: Accept loop terminated")
	
	for {
		select {
		case <-s.shutdownCh:
			return
		default:
		}
		
		// Set accept timeout to allow checking for shutdown
		if tcpListener, ok := s.listener.(*net.TCPListener); ok {
			tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
		}
		
		conn, err := s.listener.Accept()
		if err != nil {
			// Check if this is a timeout (expected during shutdown checks)
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			
			// Check if we're shutting down
			select {
			case <-s.shutdownCh:
				return
			default:
				atomic.AddInt64(&s.errors, 1)
				log.Printf("Gateway: Error accepting connection: %v", err)
				continue
			}
		}
		
		// Check if we can accept more connections
		if !s.connections.CanAccept() {
			atomic.AddInt64(&s.rejectedConnections, 1)
			log.Printf("Gateway: Rejected connection from %s - max connections reached (%d)",
				conn.RemoteAddr(), s.config.MaxConnections)
			conn.Close()
			continue
		}
		
		// Handle connection in a goroutine
		s.wg.Add(1)
		go s.handleConnection(conn)
		
		atomic.AddInt64(&s.totalConnections, 1)
	}
}

// handleConnection handles a single client connection
func (s *Server) handleConnection(clientConn net.Conn) {
	defer s.wg.Done()
	defer clientConn.Close()
	
	atomic.AddInt64(&s.activeConnections, 1)
	defer atomic.AddInt64(&s.activeConnections, -1)
	
	clientAddr := clientConn.RemoteAddr().String()
	log.Printf("Gateway: New connection from %s", clientAddr)
	
	// Track the connection
	session := s.connections.Track(clientConn)
	defer s.connections.Untrack(session.ID)
	
	// Get current master endpoint with retries for edge cases
	masterEndpoint, err := s.getMasterEndpointWithRetry(3)
	if err != nil {
		log.Printf("Gateway: Failed to get master endpoint for %s after retries: %v", clientAddr, err)
		atomic.AddInt64(&s.errors, 1)
		return
	}
	
	// Connect to master with timeout
	backendConn, err := net.DialTimeout("tcp", masterEndpoint, s.config.Timeout)
	if err != nil {
		log.Printf("Gateway: Failed to connect to master %s for client %s: %v", masterEndpoint, clientAddr, err)
		atomic.AddInt64(&s.errors, 1)
		
		// If connection to master fails, update health status
		s.healthMu.Lock()
		s.healthStatus = fmt.Sprintf("master-connection-failed: %v", err)
		s.lastHealthCheck = time.Now()
		s.healthMu.Unlock()
		
		return
	}
	defer backendConn.Close()
	
	session.SetBackendConnection(backendConn)
	log.Printf("Gateway: Proxying %s -> %s", clientAddr, masterEndpoint)
	
	// Start bidirectional proxy
	s.proxyConnections(clientConn, backendConn, session)
}

// proxyConnections handles bidirectional data transfer between client and backend
func (s *Server) proxyConnections(clientConn, backendConn net.Conn, session *ProxySession) {
	startTime := time.Now()
	clientAddr := clientConn.RemoteAddr().String()
	backendAddr := backendConn.RemoteAddr().String()
	
	// Channel to signal completion of either direction
	done := make(chan struct{}, 2)
	var clientToBackendErr, backendToClientErr error
	
	// Client -> Backend
	go func() {
		defer func() { done <- struct{}{} }()
		written, err := s.copyWithBuffer(backendConn, clientConn)
		session.AddBytesSent(written)
		clientToBackendErr = err
		if err != nil && err != io.EOF {
			log.Printf("Gateway: Error copying %s->%s: %v", clientAddr, backendAddr, err)
		}
		// Close backend write side to signal EOF
		if tcpConn, ok := backendConn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()
	
	// Backend -> Client
	go func() {
		defer func() { done <- struct{}{} }()
		written, err := s.copyWithBuffer(clientConn, backendConn)
		session.AddBytesReceived(written)
		backendToClientErr = err
		if err != nil && err != io.EOF {
			log.Printf("Gateway: Error copying %s->%s: %v", backendAddr, clientAddr, err)
		}
		// Close client write side to signal EOF
		if tcpConn, ok := clientConn.(*net.TCPConn); ok {
			tcpConn.CloseWrite()
		}
	}()
	
	// Wait for either direction to complete
	<-done
	
	// Log session summary
	duration := time.Since(startTime)
	totalBytes := session.GetBytesSent() + session.GetBytesReceived()
	
	if clientToBackendErr != nil && clientToBackendErr != io.EOF {
		atomic.AddInt64(&s.errors, 1)
	}
	if backendToClientErr != nil && backendToClientErr != io.EOF {
		atomic.AddInt64(&s.errors, 1)
	}
	
	log.Printf("Gateway: Proxy session completed %s->%s: %d bytes, %.2fs", 
		clientAddr, backendAddr, totalBytes, duration.Seconds())
}

// copyWithBuffer copies data using a buffer of configurable size
func (s *Server) copyWithBuffer(dst, src net.Conn) (int64, error) {
	buf := make([]byte, s.config.BufferSize)
	written, err := io.CopyBuffer(dst, src, buf)
	return written, err
}

// CheckHealth performs a basic health check of the gateway and master connectivity
func (s *Server) CheckHealth(ctx context.Context) string {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	
	s.lastHealthCheck = time.Now()
	
	// Check if server is running
	if atomic.LoadInt32(&s.isRunning) == 0 {
		s.healthStatus = "stopped"
		return s.healthStatus
	}
	
	// Check if master provider is working
	if s.masterProvider == nil {
		s.healthStatus = "no-master-provider"
		return s.healthStatus
	}
	
	// Try to get current master endpoint
	endpoint, err := s.masterProvider(ctx)
	if err != nil {
		s.healthStatus = fmt.Sprintf("master-unavailable: %v", err)
		return s.healthStatus
	}
	
	// Quick connectivity test to master
	conn, err := net.DialTimeout("tcp", endpoint, 5*time.Second)
	if err != nil {
		s.healthStatus = fmt.Sprintf("master-unreachable: %s (%v)", endpoint, err)
		return s.healthStatus
	}
	conn.Close()
	
	// Check error rate (if > 50% of recent connections are errors, mark as unhealthy)
	totalConns := atomic.LoadInt64(&s.totalConnections)
	errors := atomic.LoadInt64(&s.errors)
	if totalConns > 10 && float64(errors)/float64(totalConns) > 0.5 {
		s.healthStatus = "high-error-rate"
		return s.healthStatus
	}
	
	s.healthStatus = "healthy"
	return s.healthStatus
}

// getMasterEndpointWithRetry attempts to get master endpoint with retries for edge cases
func (s *Server) getMasterEndpointWithRetry(maxRetries int) (string, error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
		
		endpoint, err := s.masterProvider(ctx)
		cancel()
		
		if err == nil {
			return endpoint, nil
		}
		
		// Log the error and retry if not the last attempt
		if attempt < maxRetries {
			log.Printf("Gateway: Master endpoint lookup failed (attempt %d/%d): %v, retrying...", attempt, maxRetries, err)
			time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
		} else {
			log.Printf("Gateway: Master endpoint lookup failed (final attempt %d/%d): %v", attempt, maxRetries, err)
		}
	}
	
	return "", fmt.Errorf("failed to get master endpoint after %d attempts", maxRetries)
}

// GetStats returns current gateway statistics
func (s *Server) GetStats() GatewayStats {
	s.healthMu.RLock()
	healthStatus := s.healthStatus
	lastHealthCheck := s.lastHealthCheck.Format(time.RFC3339)
	s.healthMu.RUnlock()
	
	return GatewayStats{
		IsRunning:           atomic.LoadInt32(&s.isRunning) == 1,
		ActiveConnections:   atomic.LoadInt64(&s.activeConnections),
		TotalConnections:    atomic.LoadInt64(&s.totalConnections),
		RejectedConnections: atomic.LoadInt64(&s.rejectedConnections),
		Errors:              atomic.LoadInt64(&s.errors),
		Failovers:           atomic.LoadInt64(&s.failovers),
		CurrentMaster:       s.GetCurrentMaster(),
		HealthStatus:        healthStatus,
		LastHealthCheck:     lastHealthCheck,
	}
}

// GatewayStats holds gateway server statistics
type GatewayStats struct {
	IsRunning           bool   `json:"isRunning"`
	ActiveConnections   int64  `json:"activeConnections"`
	TotalConnections    int64  `json:"totalConnections"`
	RejectedConnections int64  `json:"rejectedConnections"`
	Errors              int64  `json:"errors"`
	Failovers           int64  `json:"failovers"`
	CurrentMaster       string `json:"currentMaster"`
	HealthStatus        string `json:"healthStatus"`
	LastHealthCheck     string `json:"lastHealthCheck"`
}