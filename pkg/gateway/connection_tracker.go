package gateway

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ConnectionTracker manages active gateway connections
type ConnectionTracker struct {
	maxConnections int
	sessions       map[string]*ProxySession
	mu             sync.RWMutex
	nextID         uint64
}

// ProxySession represents a single client connection and its proxy state
type ProxySession struct {
	ID           string
	ClientConn   net.Conn
	ClientAddr   string
	BackendConn  net.Conn
	StartTime    time.Time
	BytesSent    int64
	BytesReceived int64
	
	// Connection state
	isActive     bool
	mu           sync.RWMutex
}

// NewConnectionTracker creates a new connection tracker
func NewConnectionTracker(maxConnections int) *ConnectionTracker {
	return &ConnectionTracker{
		maxConnections: maxConnections,
		sessions:      make(map[string]*ProxySession),
	}
}

// CanAccept returns true if the tracker can accept a new connection
func (ct *ConnectionTracker) CanAccept() bool {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return len(ct.sessions) < ct.maxConnections
}

// Track adds a new connection to the tracker and returns its session
func (ct *ConnectionTracker) Track(clientConn net.Conn) *ProxySession {
	sessionID := ct.generateSessionID()
	
	session := &ProxySession{
		ID:         sessionID,
		ClientConn: clientConn,
		ClientAddr: clientConn.RemoteAddr().String(),
		StartTime:  time.Now(),
		isActive:   true,
	}
	
	ct.mu.Lock()
	ct.sessions[sessionID] = session
	ct.mu.Unlock()
	
	return session
}

// Untrack removes a connection from the tracker
func (ct *ConnectionTracker) Untrack(sessionID string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	
	if session, exists := ct.sessions[sessionID]; exists {
		session.Close()
		delete(ct.sessions, sessionID)
	}
}

// GetSession returns a session by ID
func (ct *ConnectionTracker) GetSession(sessionID string) (*ProxySession, bool) {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	
	session, exists := ct.sessions[sessionID]
	return session, exists
}

// GetAllSessions returns a copy of all active sessions
func (ct *ConnectionTracker) GetAllSessions() []*ProxySession {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	
	sessions := make([]*ProxySession, 0, len(ct.sessions))
	for _, session := range ct.sessions {
		sessions = append(sessions, session)
	}
	return sessions
}

// GetCount returns the current number of tracked connections
func (ct *ConnectionTracker) GetCount() int {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	return len(ct.sessions)
}

// CloseAll closes all tracked connections
func (ct *ConnectionTracker) CloseAll() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	
	for sessionID, session := range ct.sessions {
		session.Close()
		delete(ct.sessions, sessionID)
	}
}

// CleanupStale removes sessions that have been inactive for too long
func (ct *ConnectionTracker) CleanupStale(maxAge time.Duration) int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	
	now := time.Now()
	cleaned := 0
	
	for sessionID, session := range ct.sessions {
		if !session.IsActive() && now.Sub(session.StartTime) > maxAge {
			session.Close()
			delete(ct.sessions, sessionID)
			cleaned++
		}
	}
	
	return cleaned
}

// generateSessionID generates a unique session ID
func (ct *ConnectionTracker) generateSessionID() string {
	id := atomic.AddUint64(&ct.nextID, 1)
	return fmt.Sprintf("session-%d", id)
}

// ProxySession methods

// SetBackendConnection sets the backend connection for this session
func (ps *ProxySession) SetBackendConnection(conn net.Conn) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.BackendConn = conn
}

// GetBackendConnection returns the backend connection for this session
func (ps *ProxySession) GetBackendConnection() net.Conn {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.BackendConn
}

// IsActive returns true if the session is active
func (ps *ProxySession) IsActive() bool {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return ps.isActive
}

// AddBytesSent atomically adds to the bytes sent counter
func (ps *ProxySession) AddBytesSent(bytes int64) {
	atomic.AddInt64(&ps.BytesSent, bytes)
}

// AddBytesReceived atomically adds to the bytes received counter
func (ps *ProxySession) AddBytesReceived(bytes int64) {
	atomic.AddInt64(&ps.BytesReceived, bytes)
}

// GetBytesSent returns the total bytes sent
func (ps *ProxySession) GetBytesSent() int64 {
	return atomic.LoadInt64(&ps.BytesSent)
}

// GetBytesReceived returns the total bytes received
func (ps *ProxySession) GetBytesReceived() int64 {
	return atomic.LoadInt64(&ps.BytesReceived)
}

// Close closes both client and backend connections
func (ps *ProxySession) Close() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	
	if !ps.isActive {
		return
	}
	
	ps.isActive = false
	
	if ps.ClientConn != nil {
		ps.ClientConn.Close()
	}
	
	if ps.BackendConn != nil {
		ps.BackendConn.Close()
	}
}

// GetDuration returns how long the session has been active
func (ps *ProxySession) GetDuration() time.Duration {
	return time.Since(ps.StartTime)
}

// GetStats returns session statistics
func (ps *ProxySession) GetStats() SessionStats {
	return SessionStats{
		ID:            ps.ID,
		ClientAddr:    ps.ClientAddr,
		StartTime:     ps.StartTime,
		Duration:      ps.GetDuration(),
		BytesSent:     ps.GetBytesSent(),
		BytesReceived: ps.GetBytesReceived(),
		IsActive:      ps.IsActive(),
	}
}

// SessionStats holds statistics for a single session
type SessionStats struct {
	ID            string        `json:"id"`
	ClientAddr    string        `json:"clientAddr"`
	StartTime     time.Time     `json:"startTime"`
	Duration      time.Duration `json:"duration"`
	BytesSent     int64         `json:"bytesSent"`
	BytesReceived int64         `json:"bytesReceived"`
	IsActive      bool          `json:"isActive"`
}