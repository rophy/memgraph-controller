package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"memgraph-controller/pkg/common"
	"memgraph-controller/pkg/controller"

	"k8s.io/client-go/kubernetes"
)

func main() {
	// Load configuration first
	config, err := common.Load()
	if err != nil {
		panic(err)
	}
	
	// Initialize structured logging
	common.InitLogger()
	logger := common.GetLogger()

	logger.Info("Starting Memgraph Controller with HA support")
	logger.Info("Configuration loaded",
		"app_name", config.AppName,
		"namespace", config.Namespace,
		"reconcile_interval", config.ReconcileInterval,
		"http_port", config.HTTPPort,
	)

	k8sConfig, err := controller.GetKubernetesConfig()
	if err != nil {
		logger.Error("Failed to get Kubernetes config", "error", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.Error("Failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}

	ctrl := controller.NewMemgraphController(clientset, config)

	if err := ctrl.TestConnection(); err != nil {
		logger.Error("Failed to connect to Kubernetes API", "error", err)
		os.Exit(1)
	}

	logger.Info("Memgraph Controller created successfully")

	// Set up graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Initialize all controller components
	if err := ctrl.Initialize(ctx); err != nil {
		logger.Error("Failed to initialize controller", "error", err)
		os.Exit(1)
	}

	// Start reconciliation loop
	done := make(chan struct{})
	go func() {
		defer close(done)
		logger.Info("Starting reconciliation loop")
		if err := ctrl.Run(ctx); err != nil && err != context.Canceled {
			logger.Error("Controller reconciliation loop failed", "error", err)
			cancel()
		}
	}()

	// Wait for shutdown signal
	go func() {
		<-sigChan
		logger.Info("Received shutdown signal, stopping controller")

		// Stop all components gracefully
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()

		ctrl.Shutdown(shutdownCtx)
		cancel()
	}()

	// Wait for reconciliation loop to finish
	<-done
	logger.Info("Controller shutdown complete")
}
