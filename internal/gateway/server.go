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

	"memgraph-controller/internal/common"
	"memgraph-controller/internal/metrics"
)

// UpstreamFailureCallback is called when upstream connection failures are detected
type UpstreamFailureCallback func(ctx context.Context, upstreamAddress string, err error)

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

// Server represents the gateway server that proxies Bolt protocol connections
type Server struct {
	config      *Config
	listener    net.Listener
	connections *ConnectionTracker

	// Name of the server
	name string

	// Server state
	isRunning         int32  // atomic boolean
	upstreamAddress   string // The address of the upstream Memgraph node (e.g. main pod)
	upstreamAddressMu sync.RWMutex
	shutdownCh        chan struct{}
	wg                sync.WaitGroup

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

	// Prometheus metrics
	promMetrics *metrics.Metrics

	// Upstream failure callback for controller notification
	upstreamFailureCallback UpstreamFailureCallback
}

// NewServer creates a new gateway server with the given configuration
func NewServer(name string, config *Config) *Server {
	// Create rate limiter
	rateLimiter := NewRateLimiter(
		config.RateLimitEnabled,
		config.RateLimitRPS,
		config.RateLimitBurst,
		config.RateLimitWindow,
	)

	return &Server{
		name:            name,
		config:          config,
		connections:     NewConnectionTracker(config.MaxConnections),
		upstreamAddress: "",
		shutdownCh:      make(chan struct{}),
		healthStatus:    "unknown",
		rateLimiter:     rateLimiter,
	}
}

// Start starts the gateway server and begins accepting connections
func (s *Server) Start(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
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

		logger.Info("Gateway server listening with TLS", "address",
			"name", s.name, "address", s.config.BindAddress, "cert_path", s.config.TLSCertPath)
	} else {
		listener, err = net.Listen("tcp", s.config.BindAddress)
		if err != nil {
			atomic.StoreInt32(&s.isRunning, 0)
			return fmt.Errorf("failed to listen on %s: %w", s.config.BindAddress, err)
		}

		logger.Info("Gateway server listening", "name", s.name, "address", s.config.BindAddress, "tls", false)
	}

	s.listener = listener

	// Start background goroutines
	s.wg.Add(3)
	go s.acceptConnections(ctx)
	go s.periodicCleanup(ctx)

	return nil
}

// GetUpstreamAddress returns the upstream address
func (s *Server) GetUpstreamAddress() string {
	s.upstreamAddressMu.RLock()
	defer s.upstreamAddressMu.RUnlock()
	return s.upstreamAddress
}

func (s *Server) SetUpstreamAddress(ctx context.Context, address string) {
	s.upstreamAddressMu.Lock()
	defer s.upstreamAddressMu.Unlock()
	logger := common.GetLoggerFromContext(ctx)
	oldAddress := s.upstreamAddress // Read directly instead of calling GetUpstreamAddress()
	if oldAddress == address {
		return
	}

	logger.Info("Changing upstream address", "name", s.name, "old_address", oldAddress, "new_address", address)
	// Updating upstream address implies disconnecting all existing connections.
	s.DisconnectAll(ctx)
	s.upstreamAddress = address
}

// SetPrometheusMetrics sets the Prometheus metrics instance
func (s *Server) SetPrometheusMetrics(m *metrics.Metrics) {
	s.promMetrics = m
}

// SetUpstreamFailureCallback sets the callback for upstream failure notifications
func (s *Server) SetUpstreamFailureCallback(callback UpstreamFailureCallback) {
	s.upstreamFailureCallback = callback
}

// acceptConnections runs the main accept loop for incoming connections
func (s *Server) acceptConnections(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
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

		// Reject connection if upstream address is not set
		if s.GetUpstreamAddress() == "" {
			atomic.AddInt64(&s.rejectedConnections, 1)
			logger.Warn("Connection rejected - upstream address not set", "name", s.name, "client_ip", clientIP, "client_addr", conn.RemoteAddr().String())
			conn.Close()
			continue
		}

		// Check rate limiting
		if !s.rateLimiter.Allow(clientIP) {
			atomic.AddInt64(&s.rateLimitRejections, 1)
			logger.Warn("Connection rate limited", "name", s.name, "client_ip", clientIP, "client_addr", conn.RemoteAddr().String())
			conn.Close()
			continue
		}

		// Check if we can accept more connections
		if !s.connections.CanAccept() {
			atomic.AddInt64(&s.rejectedConnections, 1)
			s.rateLimiter.Release(clientIP) // Release rate limit token
			logger.Warn("Connection rejected - max connections reached", "name", s.name, "client_addr", conn.RemoteAddr().String(), "max_connections", s.config.MaxConnections)
			conn.Close()
			continue
		}

		// Handle connection in a goroutine
		s.wg.Add(1)
		go s.handleConnection(ctx, conn)

		atomic.AddInt64(&s.totalConnections, 1)

		// Update Prometheus metrics
		if s.promMetrics != nil {
			s.promMetrics.RecordGatewayConnection()
		}
	}
}

// handleConnection handles a single client connection
func (s *Server) handleConnection(ctx context.Context, clientConn net.Conn) {
	logger := common.GetLoggerFromContext(ctx)
	defer s.wg.Done()
	defer clientConn.Close()

	clientAddr := clientConn.RemoteAddr().String()
	clientIP := extractClientIP(clientConn)

	// Release rate limiter token when connection ends
	defer s.rateLimiter.Release(clientIP)

	atomic.AddInt64(&s.activeConnections, 1)
	defer func() {
		atomic.AddInt64(&s.activeConnections, -1)
		// Update active connections metric
		if s.promMetrics != nil {
			s.promMetrics.UpdateGatewayConnections(int(atomic.LoadInt64(&s.activeConnections)))
		}
	}()

	// Update active connections metric
	if s.promMetrics != nil {
		s.promMetrics.UpdateGatewayConnections(int(atomic.LoadInt64(&s.activeConnections)))
	}

	logger.Debug("established", "name", s.name, "client_ip", clientIP)

	// Track the connection
	session := s.connections.Track(clientConn)
	session.SetMaxBytesLimit(s.config.MaxBytesPerConnection)
	defer s.connections.Untrack(session.ID)

	upstreamAddress := s.GetUpstreamAddress()
	backendConn, err := net.DialTimeout("tcp", upstreamAddress, s.config.ConnectionTimeout)
	if err != nil {
		logger.Error("Failed to connect to upstream",
			"client_addr", clientAddr,
			"upstream_address", upstreamAddress,
			"error", err.Error(),
			"timeout", s.config.ConnectionTimeout)

		atomic.AddInt64(&s.errors, 1)
		session.AddConnectionError()

		// If connection to upstream fails, update health status
		s.healthMu.Lock()
		s.healthStatus = fmt.Sprintf("upstream-connection-failed: %v", err)
		s.lastHealthCheck = time.Now()
		s.healthMu.Unlock()

		// Notify controller about upstream failure if callback is set
		if s.upstreamFailureCallback != nil {
			go func() {
				// Run callback in background to avoid blocking connection handling
				s.upstreamFailureCallback(ctx, upstreamAddress, err)
			}()
		}

		return
	}
	defer backendConn.Close()

	session.SetBackendConnection(backendConn)
	logger.Info("Connection established to main", "name", s.name, "client_addr", clientAddr, "session_id", session.ID)

	// Start bidirectional proxy
	s.proxyConnections(ctx, clientConn, backendConn, session)
}

// proxyConnections handles bidirectional data transfer between client and backend
func (s *Server) proxyConnections(ctx context.Context, clientConn, backendConn net.Conn, session *ProxySession) {
	logger := common.GetLoggerFromContext(ctx)
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
			logger.Warn("error copying", "name", s.name, "from", clientAddr, "to", backendAddr, "error", err)
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
			logger.Warn("error copying", "name", s.name, "from", backendAddr, "to", clientAddr, "error", err)
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

	// Update Prometheus metrics for bytes transferred
	if s.promMetrics != nil {
		s.promMetrics.RecordGatewayBytes(session.GetBytesSent(), session.GetBytesReceived())
	}

	if clientToBackendErr != nil && clientToBackendErr != io.EOF {
		atomic.AddInt64(&s.errors, 1)
		if s.promMetrics != nil {
			s.promMetrics.RecordGatewayError()
		}
	}
	if backendToClientErr != nil && backendToClientErr != io.EOF {
		atomic.AddInt64(&s.errors, 1)
		if s.promMetrics != nil {
			s.promMetrics.RecordGatewayError()
		}
	}

	logger.Info("proxy session completed",
		"name", s.name,
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

// periodicCleanup runs periodic cleanup tasks for connections
func (s *Server) periodicCleanup(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
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
				logger.Info("cleaned up idle connections", "name", s.name, "count", idleCleanup)
			}

			// Clean up stale connections (fallback)
			staleCleanup := s.connections.CleanupStale(s.config.IdleTimeout * 2)
			if staleCleanup > 0 {
				logger.Info("cleaned up stale connections", "name", s.name, "count", staleCleanup)
			}

			// Log connection statistics
			activeCount := s.connections.GetCount()
			totalSent, totalReceived := s.connections.GetTotalBytes()

			if activeCount > 0 {
				logger.Info("connections", "name", s.name, "active_count", activeCount, "bytes_sent", totalSent, "bytes_received", totalReceived)
			}
		}
	}
}

// DisconnectAll disconnects all active connections
func (s *Server) DisconnectAll(ctx context.Context) {
	logger := common.GetLoggerFromContext(ctx)
	activeConnections := s.connections.GetCount()
	if activeConnections == 0 {
		logger.Info("no active connections to disconnect", "name", s.name)
		return
	}

	logger.Info("disconnecting all active connections", "name", s.name, "count", activeConnections)

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
