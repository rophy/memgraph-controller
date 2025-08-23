package controller

import (
	"context"
	"fmt"
	
	"memgraph-controller/pkg/gateway"
)

// GatewayAdapter adapts the gateway.Server to the GatewayServerInterface
type GatewayAdapter struct {
	server *gateway.Server
	config *Config
}

// NewGatewayAdapter creates a new gateway adapter
func NewGatewayAdapter(config *Config) *GatewayAdapter {
	return &GatewayAdapter{
		server: nil, // Will be initialized later with master provider
		config: config,
	}
}

// InitializeWithMasterProvider initializes the gateway server with master endpoint provider
func (g *GatewayAdapter) InitializeWithMasterProvider(masterProvider gateway.MasterEndpointProvider) error {
	if !g.config.GatewayEnabled {
		return nil // Gateway is disabled
	}
	
	gatewayConfig := gateway.LoadGatewayConfig()
	
	// Override with controller configuration
	gatewayConfig.Enabled = g.config.GatewayEnabled
	gatewayConfig.BindAddress = g.config.GatewayBindAddress
	
	g.server = gateway.NewServer(gatewayConfig, masterProvider)
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

// SetCurrentMaster updates the current master endpoint
func (g *GatewayAdapter) SetCurrentMaster(endpoint string) {
	if g.server == nil {
		return // Gateway is disabled
	}
	g.server.SetCurrentMaster(endpoint)
}

// GetCurrentMaster returns the current master endpoint
func (g *GatewayAdapter) GetCurrentMaster() string {
	if g.server == nil {
		return ""
	}
	return g.server.GetCurrentMaster()
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