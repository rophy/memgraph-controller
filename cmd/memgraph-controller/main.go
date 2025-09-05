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
	log.Println("Starting Memgraph Controller with HA support...")

	config := controller.LoadConfig()
	log.Printf("Configuration: AppName=%s, Namespace=%s, ReconcileInterval=%s, HTTPPort=%s",
		config.AppName, config.Namespace, config.ReconcileInterval, config.HTTPPort)

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

	log.Println("Memgraph Controller created successfully")

	// Set up graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Initialize all controller components
	if err := ctrl.Initialize(ctx); err != nil {
		log.Fatalf("Failed to initialize controller: %v", err)
	}

	// Start reconciliation loop
	done := make(chan struct{})
	go func() {
		defer close(done)
		log.Println("Starting reconciliation loop...")
		if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
			log.Printf("Controller reconciliation loop failed: %v", err)
			cancel()
		}
	}()

	// Wait for shutdown signal
	go func() {
		<-sigChan
		log.Println("Received shutdown signal, stopping controller...")
		
		// Stop all components gracefully
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		
		// Stop HTTP server
		if err := ctrl.StopHTTPServer(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
		
		// Stop gateway server
		if err := ctrl.StopGatewayServer(shutdownCtx); err != nil {
			log.Printf("Gateway server shutdown error: %v", err)
		}
		
		// Stop informers
		ctrl.StopInformers()
		
		cancel()
	}()

	// Wait for reconciliation loop to finish
	<-done
	log.Println("Controller shutdown complete")
}



