# Variables
REGISTRY ?= ghcr.io/rophy
TAG ?= latest
IMAGE_NAME ?= memgraph-controller

# Default target - show help
.DEFAULT_GOAL := help

.PHONY: help build test test-e2e clean up run down docker-build docker-push check wait

help: ## Show this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

# Build targets
build: ## Build the memgraph-controller binary
	mkdir -p bin
	go build -o bin/memgraph-controller ./cmd/memgraph-controller

clean: ## Remove build artifacts
	rm -rf bin/
	rm -rf logs/*
	rm -rf tests/e2e/__pycache__/
	rm -rf tests/e2e/.pytest_cache/

# Test targets
test: ## Run unit tests
	go test -v ./...

test-e2e: ## Run E2E tests (supports ARGS="test_file.py" for single tests)
	@echo "Running E2E tests..."
	./tests/scripts/run-e2e-tests.sh $(ARGS)


run: ## Deploy full memgraph-ha cluster with controller (background)
	skaffold run

down: ## Remove all skaffold resources and PVCs
	skaffold delete
	kubectl delete pvc --all -n memgraph
	kubectl delete networkpolicies --all -n memgraph
	rm -rf logs/*

check: ## Run project checks and validations
	scripts/check.sh

wait: ## Wait for cluster convergence using enhanced wait function
	scripts/wait.sh

# Docker targets
docker-build: ## Build Docker image locally
	docker build -t $(IMAGE_NAME):$(TAG) .

docker-push: docker-build ## Build and push Docker image to registry
	docker tag $(IMAGE_NAME):$(TAG) $(REGISTRY)/$(IMAGE_NAME):$(TAG)
	docker push $(REGISTRY)/$(IMAGE_NAME):$(TAG)
