package controller

import (
	"context"
	"fmt"
	"sync"

	"memgraph-controller/pkg/gateway"
)

// GatewayAdapter adapts the gateway.Server to the GatewayServerInterface
type GatewayAdapter struct {
	server         *gateway.Server
	config         *Config
	isBootstrap    bool
	bootstrapMu    sync.RWMutex
}

// NewGatewayAdapter creates a new gateway adapter
func NewGatewayAdapter(config *Config) *GatewayAdapter {
	return &GatewayAdapter{
		server:      nil, // Will be initialized later with main provider
		config:      config,
		isBootstrap: true, // Start in bootstrap phase - reject connections
	}
}

// InitializeWithMainProvider initializes the gateway server with main endpoint provider
func (g *GatewayAdapter) InitializeWithMainProvider(mainProvider gateway.MainEndpointProvider) error {
	if !g.config.GatewayEnabled {
		return nil // Gateway is disabled
	}

	gatewayConfig := gateway.LoadGatewayConfig()

	// Override with controller configuration
	gatewayConfig.Enabled = g.config.GatewayEnabled
	gatewayConfig.BindAddress = g.config.GatewayBindAddress

	// Create bootstrap phase provider that uses our internal state
	bootstrapProvider := func() bool {
		return g.IsBootstrapPhase()
	}

	g.server = gateway.NewServer(gatewayConfig, mainProvider, bootstrapProvider)
	return nil
}

// Start starts the gateway server
func (g *GatewayAdapter) Start(ctx context.Context) error {
	if g.server == nil {
		return nil // Gateway is disabled
	}
	return g.server.Start(ctx)
}

// Stop stops the gateway server
func (g *GatewayAdapter) Stop(ctx context.Context) error {
	if g.server == nil {
		return nil // Gateway is disabled
	}
	return g.server.Stop(ctx)
}

// SetCurrentMain updates the current main endpoint (no-op with dynamic provider)
func (g *GatewayAdapter) SetCurrentMain(endpoint string) {
	// No-op: Gateway now uses dynamic main provider
}

// GetCurrentMain returns the current main endpoint
func (g *GatewayAdapter) GetCurrentMain() string {
	if g.server == nil {
		return ""
	}
	return g.server.GetCurrentMain()
}

// GetStats returns gateway statistics (additional method not in interface)
func (g *GatewayAdapter) GetStats() (gateway.GatewayStats, error) {
	if g.server == nil {
		return gateway.GatewayStats{}, fmt.Errorf("gateway is disabled")
	}
	return g.server.GetStats(), nil
}

// CheckHealth performs a health check (additional method not in interface)
func (g *GatewayAdapter) CheckHealth(ctx context.Context) string {
	if g.server == nil {
		return "disabled"
	}
	return g.server.CheckHealth(ctx)
}

// GetConnectionCount returns the current number of active connections
func (g *GatewayAdapter) GetConnectionCount() int {
	if g.server == nil {
		return 0
	}
	stats := g.server.GetStats()
	return int(stats.ActiveConnections)
}

// GetTotalBytes returns total bytes transferred (sent, received)
func (g *GatewayAdapter) GetTotalBytes() (int64, int64) {
	if g.server == nil {
		return 0, 0
	}
	stats := g.server.GetStats()
	return stats.TotalBytesSent, stats.TotalBytesReceived
}

// IsHealthy returns true if the gateway is healthy
func (g *GatewayAdapter) IsHealthy(ctx context.Context) bool {
	if g.server == nil {
		return false // Disabled is considered unhealthy for monitoring
	}
	status := g.server.CheckHealth(ctx)
	return status == "healthy"
}

// SetBootstrapPhase sets the gateway to bootstrap phase (reject connections)
func (g *GatewayAdapter) SetBootstrapPhase(isBootstrap bool) {
	g.bootstrapMu.Lock()
	defer g.bootstrapMu.Unlock()
	
	if g.isBootstrap != isBootstrap {
		if isBootstrap {
			fmt.Println("=== GATEWAY BOOTSTRAP PHASE: REJECTING all client connections ===")
		} else {
			fmt.Println("=== GATEWAY OPERATIONAL PHASE: ACCEPTING client connections ===")
		}
		g.isBootstrap = isBootstrap
	}
}

// IsBootstrapPhase returns true if gateway is in bootstrap phase
func (g *GatewayAdapter) IsBootstrapPhase() bool {
	g.bootstrapMu.RLock()
	defer g.bootstrapMu.RUnlock()
	return g.isBootstrap
}
