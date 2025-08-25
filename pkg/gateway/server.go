package gateway

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// MainEndpointProvider is a function that returns the current main endpoint
type MainEndpointProvider func(ctx context.Context) (string, error)

// Server represents the gateway server that proxies Bolt protocol connections
type Server struct {
	config       *Config
	listener     net.Listener
	connections  *ConnectionTracker
	mainProvider MainEndpointProvider

	// Current main endpoint
	currentMain string
	mainMu      sync.RWMutex

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
	logger              *Logger
	rateLimitRejections int64
}

// NewServer creates a new gateway server with the given configuration
func NewServer(config *Config, mainProvider MainEndpointProvider) *Server {
	// Create rate limiter
	rateLimiter := NewRateLimiter(
		config.RateLimitEnabled,
		config.RateLimitRPS,
		config.RateLimitBurst,
		config.RateLimitWindow,
	)

	// Create structured logger
	logger := NewLogger(config.LogLevel, config.TraceEnabled)

	return &Server{
		config:       config,
		connections:  NewConnectionTracker(config.MaxConnections),
		mainProvider: mainProvider,
		shutdownCh:   make(chan struct{}),
		healthStatus: "unknown",
		rateLimiter:  rateLimiter,
		logger:       logger,
	}
}

// SetCurrentMain updates the current main endpoint that connections should be proxied to
func (s *Server) SetCurrentMain(endpoint string) {
	s.mainMu.Lock()
	defer s.mainMu.Unlock()

	if s.currentMain != endpoint {
		oldMain := s.currentMain
		s.currentMain = endpoint

		if oldMain != "" {
			// This is a failover event, terminate all existing connections
			log.Printf("Gateway: Main failover detected %s -> %s, terminating all connections", oldMain, endpoint)
			atomic.AddInt64(&s.failovers, 1)

			// Terminate all existing connections to force clients to reconnect to new main
			s.terminateAllConnections()
		} else {
			log.Printf("Gateway: Initial main endpoint set to %s", endpoint)
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

	log.Printf("Gateway: Terminating %d active connections due to main failover", activeConnections)

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

// GetCurrentMain returns the current main endpoint
func (s *Server) GetCurrentMain() string {
	s.mainMu.RLock()
	defer s.mainMu.RUnlock()
	return s.currentMain
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

		s.logger.Info("Gateway server listening with TLS", map[string]interface{}{
			"address":   s.config.BindAddress,
			"cert_path": s.config.TLSCertPath,
		})
	} else {
		listener, err = net.Listen("tcp", s.config.BindAddress)
		if err != nil {
			atomic.StoreInt32(&s.isRunning, 0)
			return fmt.Errorf("failed to listen on %s: %w", s.config.BindAddress, err)
		}

		s.logger.Info("Gateway server listening", map[string]interface{}{
			"address": s.config.BindAddress,
			"tls":     false,
		})
	}

	s.listener = listener

	// Start background goroutines
	s.wg.Add(3)
	go s.acceptConnections()
	go s.periodicCleanup()
	go s.periodicHealthCheck()

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

	// Clean up rate limiter
	s.rateLimiter.Close()

	// Wait for graceful shutdown or timeout
	select {
	case <-done:
		s.logger.Info("Gateway server shutdown complete")
		return nil
	case <-ctx.Done():
		s.logger.Warn("Gateway shutdown timeout, forcing termination")
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

		clientIP := extractClientIP(conn)

		// Check rate limiting
		if !s.rateLimiter.Allow(clientIP) {
			atomic.AddInt64(&s.rateLimitRejections, 1)
			s.logger.Warn("Connection rate limited", map[string]interface{}{
				"client_ip":   clientIP,
				"client_addr": conn.RemoteAddr().String(),
			})
			conn.Close()
			continue
		}

		// Check if we can accept more connections
		if !s.connections.CanAccept() {
			atomic.AddInt64(&s.rejectedConnections, 1)
			s.rateLimiter.Release(clientIP) // Release rate limit token
			s.logger.Warn("Connection rejected - max connections reached", map[string]interface{}{
				"client_addr":     conn.RemoteAddr().String(),
				"max_connections": s.config.MaxConnections,
			})
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

	s.logger.LogConnectionEvent("established", clientAddr, map[string]interface{}{
		"client_ip": clientIP,
	})

	// Track the connection
	session := s.connections.Track(clientConn)
	session.SetMaxBytesLimit(s.config.MaxBytesPerConnection)
	defer s.connections.Untrack(session.ID)

	// Get current main endpoint with retries for edge cases
	mainEndpoint, err := s.getMainEndpointWithRetry(3)
	if err != nil {
		s.logger.Error("Failed to get main endpoint", map[string]interface{}{
			"client_addr": clientAddr,
			"error":       err.Error(),
			"retries":     3,
		})
		atomic.AddInt64(&s.errors, 1)
		return
	}

	// Connect to main with timeout
	backendConn, err := net.DialTimeout("tcp", mainEndpoint, s.config.ConnectionTimeout)
	if err != nil {
		s.logger.Error("Failed to connect to main", map[string]interface{}{
			"client_addr":   clientAddr,
			"main_endpoint": mainEndpoint,
			"error":         err.Error(),
			"timeout":       s.config.ConnectionTimeout,
		})
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
	s.logger.Info("Connection established to main", map[string]interface{}{
		"client_addr":   clientAddr,
		"main_endpoint": mainEndpoint,
		"session_id":    session.ID,
	})

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

	// Try to get current main endpoint
	endpoint, err := s.mainProvider(ctx)
	if err != nil {
		s.healthStatus = fmt.Sprintf("main-unavailable: %v", err)
		return s.healthStatus
	}

	// Quick connectivity test to main
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

// getMainEndpointWithRetry attempts to get main endpoint with retries for edge cases
func (s *Server) getMainEndpointWithRetry(maxRetries int) (string, error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.Timeout)

		endpoint, err := s.mainProvider(ctx)
		cancel()

		if err == nil {
			return endpoint, nil
		}

		// Log the error and retry if not the last attempt
		if attempt < maxRetries {
			log.Printf("Gateway: Main endpoint lookup failed (attempt %d/%d): %v, retrying...", attempt, maxRetries, err)
			time.Sleep(time.Duration(attempt) * time.Second) // Exponential backoff
		} else {
			log.Printf("Gateway: Main endpoint lookup failed (final attempt %d/%d): %v", attempt, maxRetries, err)
		}
	}

	return "", fmt.Errorf("failed to get main endpoint after %d attempts", maxRetries)
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
				log.Printf("Gateway: Cleaned up %d idle connections", idleCleanup)
			}

			// Clean up stale connections (fallback)
			staleCleanup := s.connections.CleanupStale(s.config.IdleTimeout * 2)
			if staleCleanup > 0 {
				log.Printf("Gateway: Cleaned up %d stale connections", staleCleanup)
			}

			// Log connection statistics
			activeCount := s.connections.GetCount()
			totalSent, totalReceived := s.connections.GetTotalBytes()

			if activeCount > 0 {
				log.Printf("Gateway: Active connections: %d, Total bytes: sent=%d, received=%d",
					activeCount, totalSent, totalReceived)
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
				log.Printf("Gateway: Health status changed: %s -> %s", lastStatus, status)
			}
		}
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
		CurrentMain:         s.GetCurrentMain(),
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
	CurrentMain         string           `json:"currentMain"`
	HealthStatus        string           `json:"healthStatus"`
	LastHealthCheck     string           `json:"lastHealthCheck"`
	TotalBytesSent      int64            `json:"totalBytesSent"`
	TotalBytesReceived  int64            `json:"totalBytesReceived"`
	TotalBytes          int64            `json:"totalBytes"`
	TLSEnabled          bool             `json:"tlsEnabled"`
	RateLimiter         RateLimiterStats `json:"rateLimiter"`
}
