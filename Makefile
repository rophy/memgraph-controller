.PHONY: build test test-e2e clean up down

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