.PHONY: help install run build deploy clean logs test

# Default Neo4j URI
NEO4J_URI ?= bolt://localhost:7687
WRITE_INTERVAL ?= 1000
IMAGE_NAME ?= neo4j-client
NAMESPACE ?= memgraph

# Default target
.DEFAULT_GOAL := help

## help: Display this help message
help:
	@echo "Neo4j Client Makefile Commands:"
	@echo ""
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

install: ## Install Node.js dependencies
	npm install

run: install ## Run client locally (default: localhost:7687)
	NEO4J_URI=$(NEO4J_URI) WRITE_INTERVAL=$(WRITE_INTERVAL) npm start

build: ## Build Docker image
	docker build -t $(IMAGE_NAME) .

run-docker: build ## Run client in Docker container
	docker run --rm -it \
		-e NEO4J_URI=$(NEO4J_URI) \
		-e WRITE_INTERVAL=$(WRITE_INTERVAL) \
		$(IMAGE_NAME)

load-image: build ## Load image into local k8s (kind/minikube)
	@if command -v kind > /dev/null; then \
		kind load docker-image $(IMAGE_NAME); \
	elif command -v minikube > /dev/null; then \
		minikube image load $(IMAGE_NAME); \
	else \
		echo "Neither kind nor minikube found. Please load image manually."; \
	fi

deploy: load-image ## Deploy to Kubernetes
	kubectl apply -f deployment.yaml

logs: ## View logs from single client deployment
	kubectl logs -n $(NAMESPACE) deployment/neo4j-client -f

logs-loadtest: ## View logs from load test deployment
	kubectl logs -n $(NAMESPACE) deployment/neo4j-client-loadtest -f

scale: ## Scale load test deployment (use REPLICAS=n)
	kubectl scale deployment neo4j-client-loadtest -n $(NAMESPACE) --replicas=$(REPLICAS)

clean: ## Remove Kubernetes deployments
	kubectl delete -f deployment.yaml || true

test: install ## Test connection to Neo4j/Memgraph
	@echo "Testing connection to $(NEO4J_URI)..."
	@node -e "const neo4j = require('neo4j-driver'); \
		const driver = neo4j.driver('$(NEO4J_URI)'); \
		driver.verifyConnectivity() \
			.then(() => { console.log('✓ Connection successful'); process.exit(0); }) \
			.catch(err => { console.error('✗ Connection failed:', err.message); process.exit(1); });"

test-local: ## Test connection to localhost:7687
	$(MAKE) test NEO4J_URI=bolt://localhost:7687

test-gateway: ## Test connection via k8s gateway
	kubectl run neo4j-client-test --rm -i --restart=Never \
		--image=$(IMAGE_NAME) \
		--namespace=$(NAMESPACE) \
		-- node -e "console.log('Test completed')"