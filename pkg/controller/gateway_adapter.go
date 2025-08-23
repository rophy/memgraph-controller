package controller

import (
	"context"
	"fmt"
	"time"
	
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
	
	gatewayConfig := &gateway.Config{
		Enabled:        g.config.GatewayEnabled,
		BindAddress:    g.config.GatewayBindAddress,
		MaxConnections: 1000, // Default value, could be made configurable
		Timeout:        30 * time.Second, // 30 second timeout, could be made configurable
		BufferSize:     32768, // 32KB buffer, could be made configurable
	}
	
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