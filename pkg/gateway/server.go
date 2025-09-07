package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	
	"memgraph-controller/pkg/common"
)

var logger = common.GetLogger()

// MemgraphNode represents a Memgraph node with pod information
type MemgraphNode struct {
	Name        string
	BoltAddress string
	Pod         *v1.Pod
}

// IsReady checks if the pod is ready
func (node *MemgraphNode) IsReady() bool {
	if node.Pod == nil {
		return false
	}
	if node.Pod.Status.Phase != v1.PodRunning {
		return false
	}
	for _, condition := range node.Pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}

// MainNodeProvider is a function that returns the current main node
type MainNodeProvider func(ctx context.Context) (*MemgraphNode, error)

// BootstrapPhaseProvider is a function that returns true if gateway should reject connections (bootstrap phase)
type BootstrapPhaseProvider func() bool

// Server represents the gateway server that proxies Bolt protocol connections
type Server struct {
	config            *Config
	listener          net.Listener
	connections       *ConnectionTracker
	mainProvider      MainNodeProvider
	bootstrapProvider BootstrapPhaseProvider

	// Server state
	isRunning  int32 // atomic boolean
	shutdownCh chan struct{}
	wg         sync.WaitGroup

	// Metrics
	totalConnections    int64
	activeConnections   int64
	rejectedConnections int64
	errors              int64
	failovers           int64

	// Health monitoring
	lastHealthCheck time.Time
	healthStatus    string
	healthMu        sync.RWMutex

	// Production features
	rateLimiter         *RateLimiter
	rateLimitRejections int64
}

// NewServer creates a new gateway server with the given configuration
func NewServer(config *Config, mainProvider MainNodeProvider, bootstrapProvider BootstrapPhaseProvider) *Server {
	// Create rate limiter
	rateLimiter := NewRateLimiter(
		config.RateLimitEnabled,
		config.RateLimitRPS,
		config.RateLimitBurst,
		config.RateLimitWindow,
	)

	return &Server{
		config:            config,
		connections:       NewConnectionTracker(config.MaxConnections),
		mainProvider:      mainProvider,
		bootstrapProvider: bootstrapProvider,
		shutdownCh:        make(chan struct{}),
		healthStatus:      "unknown",
		rateLimiter:       rateLimiter,
	}
}

// terminateAllConnections removed - unused method

// GetCurrentMain returns the current main endpoint (dynamically retrieved)
func (s *Server) GetCurrentMain() string {
	ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)
	defer cancel()

	mainNode, err := s.mainProvider(ctx)
	if err != nil || mainNode == nil {
		return ""
	}
	return mainNode.BoltAddress
}

// Start starts the gateway server and begins accepting connections
func (s *Server) Start(ctx context.Context) error {

	if err := s.config.Validate(); err != nil {
		return fmt.Errorf("gateway configuration invalid: %w", err)
	}

	if !atomic.CompareAndSwapInt32(&s.isRunning, 0, 1) {
		return fmt.Errorf("gateway server is already running")
	}

	var listener net.Listener
	var err error

	// Set up listener with optional TLS
	if s.config.TLSEnabled {
		cert, err := tls.LoadX509KeyPair(s.config.TLSCertPath, s.config.TLSKeyPath)
		if err != nil {
			atomic.StoreInt32(&s.isRunning, 0)
			return fmt.Errorf("failed to load TLS certificates: %w", err)
		}

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}

		listener, err = tls.Listen("tcp", s.config.BindAddress, tlsConfig)
		if err != nil {
			atomic.StoreInt32(&s.isRunning, 0)
			return fmt.Errorf("failed to listen with TLS on %s: %w", s.config.BindAddress, err)
		}

		logger.Info("Gateway server listening with TLS", "address", s.config.BindAddress, "cert_path", s.config.TLSCertPath)
	} else {
		listener, err = net.Listen("tcp", s.config.BindAddress)
		if err != nil {
			atomic.StoreInt32(&s.isRunning, 0)
			return fmt.Errorf("failed to listen on %s: %w", s.config.BindAddress, err)
		}

		logger.Info("Gateway server listening", "address", s.config.BindAddress, "tls", false)
	}

	s.listener = listener

	// Start background goroutines
	s.wg.Add(3)
	go s.acceptConnections()
	go s.periodicCleanup()
	go s.periodicHealthCheck()

	return nil
}

// acceptConnections runs the main accept loop for incoming connections
func (s *Server) acceptConnections() {
	defer s.wg.Done()
	defer logger.Info("accept loop terminated")

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
				logger.Warn("error accepting connection", "error", err)
				continue
			}
		}

		clientIP := extractClientIP(conn)

		// Check rate limiting
		if !s.rateLimiter.Allow(clientIP) {
			atomic.AddInt64(&s.rateLimitRejections, 1)
			logger.Warn("Connection rate limited", "client_ip", clientIP, "client_addr", conn.RemoteAddr().String())
			conn.Close()
			continue
		}

		// Check if we can accept more connections
		if !s.connections.CanAccept() {
			atomic.AddInt64(&s.rejectedConnections, 1)
			s.rateLimiter.Release(clientIP) // Release rate limit token
			logger.Warn("Connection rejected - max connections reached", "client_addr", conn.RemoteAddr().String(), "max_connections", s.config.MaxConnections)
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

	clientAddr := clientConn.RemoteAddr().String()
	clientIP := extractClientIP(clientConn)

	// Release rate limiter token when connection ends
	defer s.rateLimiter.Release(clientIP)

	atomic.AddInt64(&s.activeConnections, 1)
	defer atomic.AddInt64(&s.activeConnections, -1)

	// Check bootstrap phase - reject connections during bootstrap per DESIGN.md line 58
	if s.bootstrapProvider != nil && s.bootstrapProvider() {
		logger.Debug("rejected-bootstrap", "client_ip", clientIP, "reason", "bootstrap-phase")
		atomic.AddInt64(&s.rejectedConnections, 1)
		// Connection will be closed by defer above - implements DESIGN.md bootstrap rejection
		return
	}

	logger.Debug("established", "client_ip", clientIP)

	// Track the connection
	session := s.connections.Track(clientConn)
	session.SetMaxBytesLimit(s.config.MaxBytesPerConnection)
	defer s.connections.Untrack(session.ID)

	// Get current main node with retries for edge cases
	mainNode, err := s.getMainNodeWithRetry(3)
	if err != nil {
		logger.Error("Failed to get main node", "client_addr", clientAddr, "error", err.Error(), "retries", 3)
		atomic.AddInt64(&s.rejectedConnections, 1)
		return
	}

	// Check if main pod is ready per DESIGN.md requirement
	if !mainNode.IsReady() {
		logger.Debug("rejected-main-not-ready", "client_ip", clientIP, "main_pod", mainNode.Name, "reason", "main-pod-not-ready")
		atomic.AddInt64(&s.rejectedConnections, 1)
		return
	}

	// Connect to main with timeout
	mainEndpoint := mainNode.BoltAddress
	backendConn, err := net.DialTimeout("tcp", mainEndpoint, s.config.ConnectionTimeout)
	if err != nil {
		logger.Error("Failed to connect to main", "client_addr", clientAddr, "main_endpoint", mainEndpoint, "error", err.Error(), "timeout", s.config.ConnectionTimeout)
		atomic.AddInt64(&s.errors, 1)
		session.AddConnectionError()

		// If connection to main fails, update health status
		s.healthMu.Lock()
		s.healthStatus = fmt.Sprintf("main-connection-failed: %v", err)
		s.lastHealthCheck = time.Now()
		s.healthMu.Unlock()

		return
	}
	defer backendConn.Close()

	session.SetBackendConnection(backendConn)
	logger.Info("Connection established to main", "client_addr", clientAddr, "main_endpoint", mainEndpoint, "session_id", session.ID)

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
			logger.Warn("error copying", "from", clientAddr, "to", backendAddr, "error", err)
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
			logger.Warn("error copying", "from", backendAddr, "to", clientAddr, "error", err)
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

	logger.Info("proxy session completed",
		"client_addr", clientAddr,
		"backend_addr", backendAddr,
		"total_bytes", totalBytes,
		"duration", duration.Seconds())
}

// copyWithBuffer copies data using a buffer of configurable size
func (s *Server) copyWithBuffer(dst, src net.Conn) (int64, error) {
	buf := make([]byte, s.config.BufferSize)
	written, err := io.CopyBuffer(dst, src, buf)
	return written, err
}

// CheckHealth performs a basic health check of the gateway and main connectivity
func (s *Server) CheckHealth(ctx context.Context) string {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()

	s.lastHealthCheck = time.Now()

	// Check if server is running
	if atomic.LoadInt32(&s.isRunning) == 0 {
		s.healthStatus = "stopped"
		return s.healthStatus
	}

	// Check if main provider is working
	if s.mainProvider == nil {
		s.healthStatus = "no-main-provider"
		return s.healthStatus
	}

	// Try to get current main node
	mainNode, err := s.mainProvider(ctx)
	if err != nil {
		s.healthStatus = fmt.Sprintf("main-unavailable: %v", err)
		return s.healthStatus
	}

	if mainNode == nil {
		s.healthStatus = "main-node-null"
		return s.healthStatus
	}

	// Check if main pod is ready
	if !mainNode.IsReady() {
		s.healthStatus = fmt.Sprintf("main-pod-not-ready: %s", mainNode.Name)
		return s.healthStatus
	}

	// Quick connectivity test to main
	endpoint := mainNode.BoltAddress
	conn, err := net.DialTimeout("tcp", endpoint, s.config.ConnectionTimeout)
	if err != nil {
		s.healthStatus = fmt.Sprintf("main-unreachable: %s (%v)", endpoint, err)
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

// getMainNodeWithRetry attempts to get main node with retries for edge cases
func (s *Server) getMainNodeWithRetry(maxRetries int) (*MemgraphNode, error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)

		mainNode, err := s.mainProvider(ctx)
		cancel()

		if err == nil && mainNode != nil {
			return mainNode, nil
		}

		logger.Warn("main node lookup failed", "attempt", attempt, "max_retries", maxRetries, "error", err)
		time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
	}

	return nil, fmt.Errorf("failed to get main node after %d attempts", maxRetries)
}

// periodicCleanup runs periodic cleanup tasks for connections
func (s *Server) periodicCleanup() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			// Clean up idle connections
			idleCleanup := s.connections.CleanupIdle(s.config.IdleTimeout)
			if idleCleanup > 0 {
				logger.Info("cleaned up idle connections", "count", idleCleanup)
			}

			// Clean up stale connections (fallback)
			staleCleanup := s.connections.CleanupStale(s.config.IdleTimeout * 2)
			if staleCleanup > 0 {
				logger.Info("cleaned up stale connections", "count", staleCleanup)
			}

			// Log connection statistics
			activeCount := s.connections.GetCount()
			totalSent, totalReceived := s.connections.GetTotalBytes()

			if activeCount > 0 {
				logger.Info("connections", "active_count", activeCount, "bytes_sent", totalSent, "bytes_received", totalReceived)
			}
		}
	}
}

// periodicHealthCheck runs periodic health checks
func (s *Server) periodicHealthCheck() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.config.HealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), s.config.ConnectionTimeout)
			status := s.CheckHealth(ctx)
			cancel()

			// Log health status changes
			s.healthMu.RLock()
			lastStatus := s.healthStatus
			s.healthMu.RUnlock()

			if status != lastStatus {
				logger.Info("health status changed", "from", lastStatus, "to", status)
			}
		}
	}
}

// DisconnectAll disconnects all active connections
func (s *Server) DisconnectAll() {
	activeConnections := s.connections.GetCount()
	if activeConnections == 0 {
		logger.Info("no active connections to disconnect")
		return
	}

	logger.Info("disconnecting all active connections", "count", activeConnections)

	// Get all active sessions and close them
	sessions := s.connections.GetAllSessions()
	disconnectedCount := 0
	for _, session := range sessions {
		go func(s *ProxySession) {
			s.Close()
		}(session) // Close asynchronously to avoid blocking
		disconnectedCount++
	}

}

// GetStats returns current gateway statistics
func (s *Server) GetStats() GatewayStats {
	s.healthMu.RLock()
	healthStatus := s.healthStatus
	lastHealthCheck := s.lastHealthCheck.Format(time.RFC3339)
	s.healthMu.RUnlock()

	// Get connection statistics
	activeConnections := s.connections.GetCount()
	totalBytesSent, totalBytesReceived := s.connections.GetTotalBytes()

	// Get rate limiter statistics
	rateLimiterStats := s.rateLimiter.GetStats()

	return GatewayStats{
		IsRunning:           atomic.LoadInt32(&s.isRunning) == 1,
		ActiveConnections:   int64(activeConnections),
		TotalConnections:    atomic.LoadInt64(&s.totalConnections),
		RejectedConnections: atomic.LoadInt64(&s.rejectedConnections),
		RateLimitRejections: atomic.LoadInt64(&s.rateLimitRejections),
		Errors:              atomic.LoadInt64(&s.errors),
		Failovers:           atomic.LoadInt64(&s.failovers),
		HealthStatus:        healthStatus,
		LastHealthCheck:     lastHealthCheck,
		TotalBytesSent:      totalBytesSent,
		TotalBytesReceived:  totalBytesReceived,
		TotalBytes:          totalBytesSent + totalBytesReceived,
		TLSEnabled:          s.config.TLSEnabled,
		RateLimiter:         rateLimiterStats,
	}
}

// GatewayStats holds gateway server statistics
type GatewayStats struct {
	IsRunning           bool             `json:"isRunning"`
	ActiveConnections   int64            `json:"activeConnections"`
	TotalConnections    int64            `json:"totalConnections"`
	RejectedConnections int64            `json:"rejectedConnections"`
	RateLimitRejections int64            `json:"rateLimitRejections"`
	Errors              int64            `json:"errors"`
	Failovers           int64            `json:"failovers"`
	HealthStatus        string           `json:"healthStatus"`
	LastHealthCheck     string           `json:"lastHealthCheck"`
	TotalBytesSent      int64            `json:"totalBytesSent"`
	TotalBytesReceived  int64            `json:"totalBytesReceived"`
	TotalBytes          int64            `json:"totalBytes"`
	TLSEnabled          bool             `json:"tlsEnabled"`
	RateLimiter         RateLimiterStats `json:"rateLimiter"`
}
