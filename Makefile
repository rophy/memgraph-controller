.PHONY: up down

up:
	kubectl create namespace memgraph --dry-run=client -o yaml | kubectl apply -f -
	helm upgrade --install memgraph ./charts/memgraph --namespace memgraph

down:
	kubectl delete namespace memgraph --ignore-not-found=true