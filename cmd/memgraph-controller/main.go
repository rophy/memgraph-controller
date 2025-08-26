package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
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

	log.Println("Memgraph Controller initialized successfully")

	// Start HTTP server for status API (always running, not leader-dependent)
	if err := ctrl.StartHTTPServer(); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}

	// Set up graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup

	// Start leader election and controller loop
	wg.Add(2)

	// Start leader election
	go func() {
		defer wg.Done()
		log.Println("Starting leader election...")
		if err := ctrl.RunLeaderElection(ctx); err != nil && err != context.Canceled {
			log.Printf("Leader election failed: %v", err)
			cancel()
		}
	}()

	// Start controller loop
	go func() {
		defer wg.Done()
		log.Println("Starting controller main loop...")
		if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
			log.Printf("Controller loop failed: %v", err)
			cancel()
		}
	}()

	// Wait for shutdown signal
	go func() {
		<-sigChan
		log.Println("Received shutdown signal, stopping controller...")
		
		// Stop HTTP server with timeout
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := ctrl.StopHTTPServer(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown error: %v", err)
		}
		
		cancel()
	}()

	// Wait for all components to finish
	wg.Wait()
	log.Println("Controller shutdown complete")
}



