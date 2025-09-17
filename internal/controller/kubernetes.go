package controller

import (
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"memgraph-controller/internal/common"
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
