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

	// Set up graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// STEP 1: Start Kubernetes informers
	log.Println("Starting Kubernetes informers...")
	if err := ctrl.StartInformers(); err != nil {
		log.Fatalf("Failed to start informers: %v", err)
	}
	log.Println("Informer caches synced successfully")

	// STEP 2: Start HTTP server for status API (always running, not leader-dependent)
	log.Println("Starting HTTP server...")
	if err := ctrl.StartHTTPServer(); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
	log.Println("HTTP server started successfully on port", config.HTTPPort)

	// STEP 3: Start gateway server
	log.Println("Starting gateway server...")
	if err := ctrl.StartGatewayServer(ctx); err != nil {
		log.Fatalf("Failed to start gateway server: %v", err)
	}
	log.Println("Gateway server started successfully")

	// STEP 4: Start leader election
	log.Println("Starting leader election...")
	go func() {
		if err := ctrl.RunLeaderElection(ctx); err != nil {
			log.Printf("Leader election failed: %v", err)
			cancel()
		}
	}()
	log.Println("Leader election started successfully")

	var wg sync.WaitGroup

	// STEP 5: Start reconciliation loop
	wg.Add(1)
	go func() {
		defer wg.Done()
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

	// Wait for all components to finish
	wg.Wait()
	log.Println("Controller shutdown complete")
}



