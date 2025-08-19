package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"memgraph-controller/pkg/controller"

	"k8s.io/client-go/kubernetes"
)

func main() {
	log.Println("Starting Memgraph Controller...")

	config := controller.LoadConfig()
	log.Printf("Configuration: AppName=%s, Namespace=%s, ReconcileInterval=%s",
		config.AppName, config.Namespace, config.ReconcileInterval)

	k8sConfig, err := controller.GetKubernetesConfig()
	if err != nil {
		log.Fatalf("Failed to get Kubernetes config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	ctrl := controller.NewMemgraphController(clientset, config)

	if err := ctrl.TestConnection(); err != nil {
		log.Fatalf("Failed to connect to Kubernetes API: %v", err)
	}

	log.Println("Memgraph Controller started successfully")

	// Set up graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal, stopping controller...")
		cancel()
	}()

	// Start reconciliation loop
	ticker := time.NewTicker(config.ReconcileInterval)
	defer ticker.Stop()

	log.Printf("Starting reconciliation loop with interval: %s", config.ReconcileInterval)
	
	// Run initial reconciliation
	if err := ctrl.Reconcile(ctx); err != nil {
		log.Printf("Initial reconciliation failed: %v", err)
	}

	// Main reconciliation loop
	for {
		select {
		case <-ctx.Done():
			log.Println("Controller shutting down...")
			return
		case <-ticker.C:
			if err := ctrl.Reconcile(ctx); err != nil {
				log.Printf("Reconciliation failed: %v", err)
			}
		}
	}
}



