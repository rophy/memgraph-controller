package controller

import (
	"context"
	"fmt"
	"time"

	"memgraph-controller/internal/common"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// StartInformers starts the Kubernetes informers and waits for cache sync
func (c *MemgraphController) StartInformers(ctx context.Context) error {
	logger := common.GetLoggerFromContext(ctx)
	logger.Info("starting informers")

	// Create shared informer factory with label selector filtering
	labelSelector := fmt.Sprintf("app.kubernetes.io/name=%s", c.config.AppName)
	c.informerFactory = informers.NewSharedInformerFactoryWithOptions(
		c.clientset,
		time.Second*30, // Resync period
		informers.WithNamespace(c.config.Namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = labelSelector
		}),
	)
	// Set up pod informer - now automatically filtered to memgraph pods only
	c.podInformer = c.informerFactory.Core().V1().Pods().Informer()

	// Add event handlers for pods - no manual filtering needed
	c.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onPodAdd,
		UpdateFunc: c.onPodUpdate,
		DeleteFunc: c.onPodDelete,
	})

	c.informerFactory.Start(c.stopCh)

	// Wait for informer caches to sync
	if !cache.WaitForCacheSync(c.stopCh, c.podInformer.HasSynced) {
		return fmt.Errorf("failed to sync informer caches")
	}

	// Now that informers are synced, initialize cluster operations
	if c.cluster == nil {
		c.cluster = NewMemgraphCluster(c.podInformer.GetStore(), c.config, c.memgraphClient)
	}

	return nil
}

// StopInformers stops the Kubernetes informers
func (c *MemgraphController) StopInformers() {
	if c.stopCh != nil {
		common.GetLogger().Info("stopping informers")
		close(c.stopCh)
	}
}

// onPodAdd handles pod addition events
func (c *MemgraphController) onPodAdd(obj interface{}) {

	// Do nothing if not leader
	if !c.IsLeader() {
		return
	}

	ctx, logger := common.WithAttr(c.ctx, "event", "podAdd")

	// Do nothing if target main index is not found
	_, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		logger.Warn("onPodAdd: failed to get target main index", "error", err)
		return
	}

	pod := obj.(*v1.Pod)
	logger.Info("onPodAdd: pod added, triggering reconciliation", "pod_name", pod.Name)
	c.enqueueReconcileEvent(ctx, "pod-add", "pod-added", pod.Name)
}

// onPodUpdate handles pod update events with immediate processing for critical changes
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {

	// Do nothing if not leader
	if !c.IsLeader() {
		return
	}

	ctx, logger := common.WithAttr(c.ctx, "event", "podUpdate")

	// Do nothing if target main index is not found
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err != nil {
		logger.Warn("onPodUpdate: failed to get target main index", "error", err)
		return
	}

	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	oldPodReady := isPodReady(oldPod)
	newPodReady := isPodReady(newPod)

	logger.Debug("onPodUpdate",
		"target_main_index", targetMainIndex,
		"old_pod", oldPod.Name,
		"new_pod", newPod.Name,
		"old_pod_ready", oldPodReady,
		"new_pod_ready", newPodReady,
		"old_pod_ip", oldPod.Status.PodIP,
		"new_pod_ip", newPod.Status.PodIP,
		"old_pod_phase", oldPod.Status.Phase,
		"new_pod_phase", newPod.Status.Phase,
		"old_pod_delete_ts", oldPod.ObjectMeta.DeletionTimestamp,
		"new_pod_delete_ts", newPod.ObjectMeta.DeletionTimestamp,
	)

	// Check for IP changes and update connection pool
	if oldPod.Status.PodIP != newPod.Status.PodIP {
		if c.memgraphClient != nil && newPod.Status.PodIP != "" {
			c.memgraphClient.connectionPool.UpdatePodIP(ctx, newPod.Name, newPod.Status.PodIP)
		}
	}

	currentMain := c.config.GetPodName(targetMainIndex)
	if currentMain == newPod.Name {
		// This is the current main pod - check for immediate health issues
		if isPodBecomeUnhealthy(ctx, oldPod, newPod) {
			logger.Info("onPodUpdate: main pod became unhealthy, triggering failover check", "pod_name", newPod.Name)
			c.enqueueFailoverCheckEvent(ctx, "pod-update", "main-pod-unhealthy", newPod.Name)
		}
	} else {
		// This is not the main pod - check if it's a replica affecting read gateway
		c.handleReplicaHealthChange(ctx, oldPod, newPod)
	}

	// Fall back to regular reconciliation for non-critical changes
	if !c.shouldReconcile(oldPod, newPod) {
		return // Skip unnecessary reconciliation
	}

	logger.Info("onPodUpdate: pod state changed, triggering reconciliation", "pod_name", newPod.Name)
	c.enqueueReconcileEvent(ctx, "pod-update", "pod-state-changed", newPod.Name)
}

// onPodDelete handles pod deletion events
func (c *MemgraphController) onPodDelete(obj interface{}) {

	// Do nothing if not leader
	if !c.IsLeader() {
		return
	}

	ctx, logger := common.WithAttr(c.ctx, "event", "podDelete")

	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	// Do nothing if target main index is not found
	if err != nil {
		logger.Warn("onPodDelete: failed to get target main index", "error", err)
		return
	}

	pod := obj.(*v1.Pod)

	// Invalidate connection for deleted pod through memgraph client connection pool
	if c.memgraphClient != nil {
		c.memgraphClient.connectionPool.InvalidatePodConnection(ctx, pod.Name)
	}

	currentMain := c.config.GetPodName(targetMainIndex)

	logger.Debug("onPodDelete", "pod_name", pod.Name, "current_main", currentMain, "target_main_index", targetMainIndex)

	if currentMain != "" && pod.Name == currentMain {
		logger.Info("Main pod deletion detected - NOT triggering failover (prestop hook will manage transition)",
			"pod_name", pod.Name)
		// Don't trigger failover - prestop hook manages planned shutdown
		// Failover only for unplanned failures via onPodUpdate
	} else {
		// Check if the deleted pod was the read gateway upstream
		c.handleReplicaDeleted(ctx, pod)
	}

	// Still enqueue for reconciliation cleanup
	logger.Info("onPodDelete: pod deleted, triggering reconciliation", "pod_name", pod.Name)
	c.enqueueReconcileEvent(ctx, "pod-delete", "pod-deleted", pod.Name)
}

// shouldReconcile determines if a pod update should trigger reconciliation
func (c *MemgraphController) shouldReconcile(oldPod, newPod *v1.Pod) bool {
	// Always reconcile if pod readiness changed
	if isPodReady(oldPod) != isPodReady(newPod) {
		return true
	}

	// Always reconcile if pod IP changed
	if oldPod.Status.PodIP != newPod.Status.PodIP {
		return true
	}

	// Reconcile if pod phase changed
	if oldPod.Status.Phase != newPod.Status.Phase {
		return true
	}

	// Reconcile if deletion timestamp changed (pod being deleted)
	oldDeletion := oldPod.ObjectMeta.DeletionTimestamp != nil
	newDeletion := newPod.ObjectMeta.DeletionTimestamp != nil
	if oldDeletion != newDeletion {
		return true
	}

	// Reconcile if node assignment changed (pod migration)
	if oldPod.Spec.NodeName != newPod.Spec.NodeName {
		return true
	}

	// Skip reconciliation for minor metadata changes
	return false
}

// handleReplicaHealthChange handles health changes for replica pods and updates read gateway upstream if needed
func (c *MemgraphController) handleReplicaHealthChange(ctx context.Context, oldPod, newPod *v1.Pod) {
	logger := common.GetLoggerFromContext(ctx)

	// Skip if read gateway is not enabled
	if c.readGatewayServer == nil {
		return
	}

	// Check if this replica became unhealthy
	wasHealthy := isPodReady(oldPod)
	isHealthy := isPodReady(newPod)

	// Only act if the pod became unhealthy (healthy -> unhealthy transition)
	if !wasHealthy || isHealthy {
		return // No health degradation
	}

	logger.Info("replica pod became unhealthy, checking if read gateway upstream switch needed",
		"pod_name", newPod.Name,
		"was_healthy", wasHealthy,
		"is_healthy", isHealthy)

	// Get current read gateway upstream
	currentUpstream := c.readGatewayServer.GetUpstreamAddress()
	if currentUpstream == "" {
		logger.Debug("read gateway has no upstream set, no switching needed")
		return
	}

	// Check if the unhealthy pod is the current read gateway upstream
	unhealthyPodAddress := newPod.Status.PodIP + ":7687"
	if currentUpstream == unhealthyPodAddress {
		logger.Warn("🔄 READ GATEWAY UPSTREAM SWITCH: Current upstream became unhealthy, switching to different replica",
			"pod_name", newPod.Name,
			"upstream_address", unhealthyPodAddress,
			"action", "immediate_upstream_switch")

		// Immediately update read gateway upstream to select a different healthy replica
		c.updateReadGatewayUpstream(ctx)

		logger.Info("read gateway upstream switch completed",
			"unhealthy_pod", newPod.Name,
			"new_upstream", c.readGatewayServer.GetUpstreamAddress())
	} else {
		logger.Debug("unhealthy pod is not current read gateway upstream, no switching needed",
			"unhealthy_pod", newPod.Name,
			"unhealthy_pod_address", unhealthyPodAddress,
			"current_upstream", currentUpstream)
	}
}

// handleReplicaDeleted handles deletion of replica pods and updates read gateway upstream if needed
func (c *MemgraphController) handleReplicaDeleted(ctx context.Context, deletedPod *v1.Pod) {
	logger := common.GetLoggerFromContext(ctx)

	// Skip if read gateway is not enabled
	if c.readGatewayServer == nil {
		return
	}

	// Get current read gateway upstream
	currentUpstream := c.readGatewayServer.GetUpstreamAddress()
	if currentUpstream == "" {
		logger.Debug("read gateway has no upstream set, no switching needed for deleted pod", "pod_name", deletedPod.Name)
		return
	}

	// Check if the deleted pod was the current read gateway upstream
	deletedPodAddress := deletedPod.Status.PodIP + ":7687"
	if currentUpstream == deletedPodAddress {
		logger.Warn("🔄 READ GATEWAY UPSTREAM SWITCH: Current upstream pod deleted, switching to different replica",
			"pod_name", deletedPod.Name,
			"upstream_address", deletedPodAddress,
			"action", "immediate_upstream_switch")

		// Immediately disconnect all connections and clear upstream to prevent connection attempts to deleted pod
		c.readGatewayServer.DisconnectAll(ctx)
		c.readGatewayServer.SetUpstreamAddress(ctx, "")

		// Update read gateway upstream to select a different healthy replica
		c.updateReadGatewayUpstream(ctx)

		logger.Info("read gateway upstream switch completed for deleted pod",
			"deleted_pod", deletedPod.Name,
			"new_upstream", c.readGatewayServer.GetUpstreamAddress())
	} else {
		logger.Debug("deleted pod was not current read gateway upstream, no switching needed",
			"deleted_pod", deletedPod.Name,
			"deleted_pod_address", deletedPodAddress,
			"current_upstream", currentUpstream)
	}
}
