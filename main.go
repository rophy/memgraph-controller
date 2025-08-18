package main

import (
	"context"
	"log"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type MemgraphController struct {
	clientset kubernetes.Interface
	config    *Config
}

func main() {
	log.Println("Starting Memgraph Controller...")

	config := LoadConfig()
	log.Printf("Configuration: AppName=%s, Namespace=%s, ReconcileInterval=%s",
		config.AppName, config.Namespace, config.ReconcileInterval)

	k8sConfig, err := getKubernetesConfig()
	if err != nil {
		log.Fatalf("Failed to get Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	controller := &MemgraphController{
		clientset: clientset,
		config:    config,
	}

	if err := controller.testConnection(); err != nil {
		log.Fatalf("Failed to connect to Kubernetes API: %v", err)
	}

	log.Println("Memgraph Controller started successfully")
}

func getKubernetesConfig() (*rest.Config, error) {
	if config, err := rest.InClusterConfig(); err == nil {
		log.Println("Using in-cluster configuration")
		return config, nil
	}

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.Getenv("HOME") + "/.kube/config"
	}

	log.Printf("Using kubeconfig: %s", kubeconfig)
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func (c *MemgraphController) testConnection() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pods, err := c.clientset.CoreV1().Pods(c.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + c.config.AppName,
	})
	if err != nil {
		return err
	}

	log.Printf("Successfully connected to Kubernetes API. Found %d pods with app=%s in namespace %s",
		len(pods.Items), c.config.AppName, c.config.Namespace)

	return nil
}

