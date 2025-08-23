# Variables
REGISTRY ?= ghcr.io/rophy
TAG ?= latest
IMAGE_NAME ?= memgraph-controller

.PHONY: build test test-e2e clean up run down docker-build docker-push check

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
	cd tests && go mod tidy && go test -v ./...

test-failover:
	cd tests && go mod tidy && go test -v -run TestE2E_FailoverReliability

# Development targets
up:
	skaffold run --profile memgraph-only

run:
	skaffold run --tail --port-forward

down:
	skaffold delete
	kubectl delete pvc --all -n memgraph

check:
	scripts/check.sh

# Docker targets
docker-build:
	docker build -t $(IMAGE_NAME):$(TAG) .

docker-push: docker-build
	docker tag $(IMAGE_NAME):$(TAG) $(REGISTRY)/$(IMAGE_NAME):$(TAG)
	docker push $(REGISTRY)/$(IMAGE_NAME):$(TAG)
