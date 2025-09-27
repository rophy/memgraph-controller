package controller

import (
	"context"
	"os"

	"memgraph-controller/internal/common"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func GetKubernetesConfig() (*rest.Config, error) {
	if config, err := rest.InClusterConfig(); err == nil {
		common.GetLogger().Info("using in-cluster configuration")
		return config, nil
	}

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	common.GetLogger().Info("using kubeconfig", "kubeconfig", kubeconfig)
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// updateCachedState removed - obsolete with new architecture

// isPodBecomeUnhealthy checks if a pod transitioned from healthy to unhealthy
func isPodBecomeUnhealthy(ctx context.Context, oldPod, newPod *v1.Pod) bool {
	if oldPod == nil || newPod == nil {
		return false
	}

	oldReady := isPodReady(oldPod)
	newReady := isPodReady(newPod)

	isBecomeUnhealthy := false

	// Pod is unhealthy if it was ready before and is not ready now
	if oldReady && !newReady {
		isBecomeUnhealthy = true
	}

	// Pod is unhealthy is it newly set a deletion timestamp
	if oldPod.ObjectMeta.DeletionTimestamp == nil && newPod.ObjectMeta.DeletionTimestamp != nil {
		isBecomeUnhealthy = true
	}

	logger := common.GetLoggerFromContext(ctx)
	if isBecomeUnhealthy {
		logger.Warn("🚨 isPodBecomeUnhealthy",
			"pod", newPod.Name,
			"old_ready", oldReady,
			"new_ready", newReady,
			"is_become_unhealthy", isBecomeUnhealthy,
		)
	}
	return isBecomeUnhealthy
}

// isPodReady checks if a pod is ready based on its conditions
func isPodReady(pod *v1.Pod) bool {
	if pod.ObjectMeta.DeletionTimestamp != nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == v1.PodReady {
			return condition.Status == v1.ConditionTrue
		}
	}
	return false
}
