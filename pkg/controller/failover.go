package controller

import (
	"log"
)

// handleMainFailurePromotion handles main failure and promotes SYNC replica

// identifyFailedMainIndex determines which pod (0 or 1) was the failed main
func (c *MemgraphController) identifyFailedMainIndex(cluster *MemgraphCluster) int {
	pod0Name := c.config.StatefulSetName + "-0"
	pod1Name := c.config.StatefulSetName + "-1"

	// Check which pod has the most recent restart (indicating it was the failed main)
	pod0Info, pod0Exists := cluster.MemgraphNodes[pod0Name]
	pod1Info, pod1Exists := cluster.MemgraphNodes[pod1Name]

	if !pod0Exists && !pod1Exists {
		log.Printf("Warning: Neither pod-0 nor pod-1 found, defaulting to pod-0 as failed main")
		return 0
	}

	if !pod0Exists {
		return 0 // pod-0 doesn't exist, so it failed
	}

	if !pod1Exists {
		return 1 // pod-1 doesn't exist, so it failed
	}

	// Both exist - check which has newer timestamp (more recent restart)
	// The pod that restarted more recently is likely the failed main
	if pod0Info.Timestamp.After(pod1Info.Timestamp) {
		log.Printf("pod-0 has newer timestamp (%v vs %v) - likely the failed main",
			pod0Info.Timestamp, pod1Info.Timestamp)
		return 0
	} else {
		log.Printf("pod-1 has newer timestamp (%v vs %v) - likely the failed main",
			pod1Info.Timestamp, pod0Info.Timestamp)
		return 1
	}
}

