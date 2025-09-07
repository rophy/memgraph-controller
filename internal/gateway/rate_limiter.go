package gateway

import (
	"net"
	"sync"
	"time"
)

// RateLimiter manages connection rate limiting per client IP
type RateLimiter struct {
	enabled    bool
	rps        int
	burst      int
	window     time.Duration
	clients    map[string]*ClientLimiter
	mu         sync.RWMutex
	cleanupTicker *time.Ticker
	stopCh     chan struct{}
}

// ClientLimiter tracks rate limiting state for a single client IP
type ClientLimiter struct {
	tokens     int
	lastRefill time.Time
	connections int
	mu         sync.Mutex
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(enabled bool, rps, burst int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		enabled: enabled,
		rps:     rps,
		burst:   burst,
		window:  window,
		clients: make(map[string]*ClientLimiter),
		stopCh:  make(chan struct{}),
	}
	
	if enabled {
		// Start cleanup goroutine to remove inactive clients
		rl.cleanupTicker = time.NewTicker(window * 2)
		go rl.cleanupRoutine()
	}
	
	return rl
}

// Allow checks if a connection from the given client IP should be allowed
func (rl *RateLimiter) Allow(clientIP string) bool {
	if !rl.enabled {
		return true
	}
	
	rl.mu.Lock()
	client, exists := rl.clients[clientIP]
	if !exists {
		client = &ClientLimiter{
			tokens:     rl.burst,
			lastRefill: time.Now(),
			connections: 0,
		}
		rl.clients[clientIP] = client
	}
	rl.mu.Unlock()
	
	client.mu.Lock()
	defer client.mu.Unlock()
	
	// Refill tokens based on time passed
	now := time.Now()
	timePassed := now.Sub(client.lastRefill)
	tokensToAdd := int(timePassed.Seconds()) * rl.rps
	
	if tokensToAdd > 0 {
		client.tokens += tokensToAdd
		if client.tokens > rl.burst {
			client.tokens = rl.burst
		}
		client.lastRefill = now
	}
	
	// Check if connection is allowed
	if client.tokens > 0 {
		client.tokens--
		client.connections++
		return true
	}
	
	return false
}

// Release decrements the connection count for a client IP
func (rl *RateLimiter) Release(clientIP string) {
	if !rl.enabled {
		return
	}
	
	rl.mu.RLock()
	client, exists := rl.clients[clientIP]
	rl.mu.RUnlock()
	
	if exists {
		client.mu.Lock()
		if client.connections > 0 {
			client.connections--
		}
		client.mu.Unlock()
	}
}

// GetStats returns rate limiter statistics
func (rl *RateLimiter) GetStats() RateLimiterStats {
	if !rl.enabled {
		return RateLimiterStats{
			Enabled: false,
		}
	}
	
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	
	activeClients := 0
	totalConnections := 0
	
	for _, client := range rl.clients {
		client.mu.Lock()
		if client.connections > 0 {
			activeClients++
		}
		totalConnections += client.connections
		client.mu.Unlock()
	}
	
	return RateLimiterStats{
		Enabled:          true,
		RPS:              rl.rps,
		Burst:            rl.burst,
		Window:           rl.window,
		TrackedClients:   len(rl.clients),
		ActiveClients:    activeClients,
		TotalConnections: totalConnections,
	}
}

// Close shuts down the rate limiter
func (rl *RateLimiter) Close() {
	if rl.enabled && rl.cleanupTicker != nil {
		close(rl.stopCh)
		rl.cleanupTicker.Stop()
	}
}

// cleanupRoutine removes inactive clients from memory
func (rl *RateLimiter) cleanupRoutine() {
	for {
		select {
		case <-rl.stopCh:
			return
		case <-rl.cleanupTicker.C:
			rl.cleanupInactiveClients()
		}
	}
}

// cleanupInactiveClients removes clients with no active connections and stale tokens
func (rl *RateLimiter) cleanupInactiveClients() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	
	now := time.Now()
	for ip, client := range rl.clients {
		client.mu.Lock()
		// Remove clients with no active connections and haven't been seen in 2x the window
		if client.connections == 0 && now.Sub(client.lastRefill) > rl.window*2 {
			delete(rl.clients, ip)
		}
		client.mu.Unlock()
	}
}

// extractClientIP extracts the client IP from a network connection
func extractClientIP(conn net.Conn) string {
	if conn == nil {
		return "unknown"
	}
	
	if addr := conn.RemoteAddr(); addr != nil {
		host, _, err := net.SplitHostPort(addr.String())
		if err != nil {
			return addr.String()
		}
		return host
	}
	
	return "unknown"
}

// RateLimiterStats holds rate limiter statistics
type RateLimiterStats struct {
	Enabled          bool          `json:"enabled"`
	RPS              int           `json:"rps"`
	Burst            int           `json:"burst"`
	Window           time.Duration `json:"window"`
	TrackedClients   int           `json:"trackedClients"`
	ActiveClients    int           `json:"activeClients"`
	TotalConnections int           `json:"totalConnections"`
}