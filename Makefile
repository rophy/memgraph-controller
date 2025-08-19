# Variables
REGISTRY ?= ghcr.io/rophy
TAG ?= latest
IMAGE_NAME ?= memgraph-controller

.PHONY: build test test-e2e clean up down docker-build docker-push

# Build targets
build:
	mkdir -p bin
	go build -o bin/memgraph-controller ./cmd/memgraph-controller

clean:
	rm -rf bin/

# Test targets
test:
	go test -v ./...

test-e2e:
	go test -v -tags=e2e ./...

# Development targets
up:
	kubectl create namespace memgraph --dry-run=client -o yaml | kubectl apply -f -
	helm upgrade --install memgraph ./charts/memgraph --namespace memgraph

down:
	kubectl delete namespace memgraph --ignore-not-found=true

# Docker targets
docker-build:
	docker build -t $(IMAGE_NAME):$(TAG) .

docker-push: docker-build
	docker tag $(IMAGE_NAME):$(TAG) $(REGISTRY)/$(IMAGE_NAME):$(TAG)
	docker push $(REGISTRY)/$(IMAGE_NAME):$(TAG)