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
	// Get target main index and delegate to cluster implementation
	targetMainIndex, err := c.GetTargetMainIndex(context.Background())
	if err != nil {
		log.Printf("Warning: failed to get target main index for deterministic roles, using 0: %v", err)
		targetMainIndex = 0
	}
	cluster.applyDeterministicRoles(targetMainIndex)
}

// learnExistingTopology discovers the current cluster topology from Memgraph
func (c *MemgraphController) learnExistingTopology(cluster *MemgraphCluster) {
	// Get current target main index and delegate to cluster implementation
	targetMainIndex, err := c.GetTargetMainIndex(context.Background())
	if err != nil {
		log.Printf("Warning: failed to get target main index for learn topology, using 0: %v", err)
		targetMainIndex = 0
	}
	
	cluster.learnExistingTopology(targetMainIndex)
	
	// Update target main index if cluster discovered a different main
	if cluster.CurrentMain != "" {
		discoveredMainIndex := c.config.ExtractPodIndex(cluster.CurrentMain)
		if discoveredMainIndex >= 0 && discoveredMainIndex != targetMainIndex {
			if err := c.SetTargetMainIndex(context.Background(), discoveredMainIndex); err != nil {
				log.Printf("Warning: failed to update target main index to %d: %v", discoveredMainIndex, err)
			} else {
				log.Printf("Updated target main index to match discovered main: %d", discoveredMainIndex)
			}
		}
	}
}

// selectMainAfterQuerying selects the main pod after querying Memgraph states
func (c *MemgraphController) selectMainAfterQuerying(ctx context.Context, cluster *MemgraphCluster) {
	// Get target main index and delegate to cluster implementation
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		log.Printf("Warning: failed to get target main index for select main, using 0: %v", err)
		targetMainIndex = 0
	}
	cluster.selectMainAfterQuerying(ctx, targetMainIndex)
}