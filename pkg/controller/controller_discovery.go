package controller

import (
	"context"
	"fmt"
	"log"
)

// discoverClusterAndCreateConfigMap implements DESIGN.md "Discover Cluster State" section
func (c *MemgraphController) discoverClusterAndCreateConfigMap(ctx context.Context) error {
	log.Println("=== CLUSTER DISCOVERY ===")

	// Use bootstrap logic to discover and initialize cluster
	bootstrapController := NewBootstrapController(c)
	_, err := bootstrapController.ExecuteBootstrap(ctx)
	if err != nil {
		return fmt.Errorf("cluster discovery failed: %w", err)
	}

	log.Printf("✅ Cluster discovery completed - main: %s", c.cluster.CurrentMain)
	return nil
}

// applyDeterministicRoles applies deterministic role assignment per DESIGN.md
func (c *MemgraphController) applyDeterministicRoles(cluster *MemgraphCluster) {
	// Delegate to cluster implementation
	cluster.applyDeterministicRoles()
}

// learnExistingTopology discovers the current cluster topology from Memgraph
func (c *MemgraphController) learnExistingTopology(cluster *MemgraphCluster) {
	// Delegate to cluster implementation
	cluster.learnExistingTopology()
}

// selectMainAfterQuerying selects the main pod after querying Memgraph states
func (c *MemgraphController) selectMainAfterQuerying(ctx context.Context, cluster *MemgraphCluster) {
	// Delegate to cluster implementation
	cluster.selectMainAfterQuerying(ctx)
}