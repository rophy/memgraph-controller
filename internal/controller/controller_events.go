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

	if !c.IsLeader() {
		return
	}

	ctx, logger := common.WithAttr(c.ctx, "event", "podAdd")

	pod := obj.(*v1.Pod)
	// No manual filtering needed - informer is already filtered to memgraph pods
	c.enqueueReconcileEvent(ctx, "pod-add", "pod-added", pod.Name)
	logger.Info("🚨 POD ADDED - triggering reconciliation", "pod_name", pod.Name)
}

// onPodUpdate handles pod update events with immediate processing for critical changes
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {

	if !c.IsLeader() {
		return
	}

	ctx, logger := common.WithAttr(c.ctx, "event", "podUpdate")

	oldPod := oldObj.(*v1.Pod)
	newPod := newObj.(*v1.Pod)

	// No manual filtering needed - informer is already filtered to memgraph pods

	// Check for IP changes and update connection pool
	if oldPod.Status.PodIP != newPod.Status.PodIP {
		if c.memgraphClient != nil && newPod.Status.PodIP != "" {
			c.memgraphClient.connectionPool.UpdatePodIP(newPod.Name, newPod.Status.PodIP)
		}
	}

	// IMMEDIATE ANALYSIS: Check for critical main pod health changes
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	if err == nil {
		currentMain := c.config.GetPodName(targetMainIndex)
		if currentMain == newPod.Name {
			// This is the current main pod - check for immediate health issues
			if c.isPodBecomeUnhealthy(oldPod, newPod) {
				logger.Info("🚨 IMMEDIATE EVENT: Main pod became unhealthy, triggering failover check", "pod_name", newPod.Name)

				// Queue failover check event
				c.enqueueFailoverCheckEvent(ctx, "pod-update", "main-pod-unhealthy", newPod.Name)
				logger.Info("🚨 IMMEDIATE EVENT: Main pod became unhealthy, triggering failover check", "pod_name", newPod.Name)

			}
		}
	}

	// Fall back to regular reconciliation for non-critical changes
	if !c.shouldReconcile(oldPod, newPod) {
		return // Skip unnecessary reconciliation
	}

	c.enqueueReconcileEvent(ctx, "pod-update", "pod-state-changed", newPod.Name)
	logger.Info("🚨 POD STATE CHANGED - triggering reconciliation", "pod_name", newPod.Name)
}

// onPodDelete handles pod deletion events
func (c *MemgraphController) onPodDelete(obj interface{}) {

	if !c.IsLeader() {
		return
	}

	ctx, logger := common.WithAttr(c.ctx, "event", "podDelete")

	pod := obj.(*v1.Pod)

	// Invalidate connection for deleted pod through memgraph client connection pool
	if c.memgraphClient != nil {
		c.memgraphClient.connectionPool.InvalidatePodConnection(pod.Name)
	}

	// Check if the deleted pod is the current main - trigger IMMEDIATE failover
	targetMainIndex, err := c.GetTargetMainIndex(ctx)
	currentMain := ""
	if err != nil {
		logger.Info("Could not get current main from target index", "error", err)
	} else {
		currentMain = c.config.GetPodName(targetMainIndex)
	}

	if currentMain != "" && pod.Name == currentMain {
		logger.Info("🚨 MAIN POD DELETED - triggering failover check", "pod_name", pod.Name)

		// Queue failover check event - only processed by leader
		c.enqueueFailoverCheckEvent(ctx, "pod-delete", "main-pod-deleted", pod.Name)
		logger.Info("🚨 MAIN POD DELETED - triggering failover check", "pod_name", pod.Name)
	}

	// Still enqueue for reconciliation cleanup
	c.enqueueReconcileEvent(ctx, "pod-delete", "pod-deleted", pod.Name)
	logger.Info("🚨 POD DELETED - triggering reconciliation", "pod_name", pod.Name)
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
