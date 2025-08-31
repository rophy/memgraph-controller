.PHONY: init clean status logs check

RELEASE_NAME ?= memgraph-sandbox

init:
	@echo "Initializing memgraph sandbox cluster..."
	@./scripts/init.sh $(RELEASE_NAME)

clean:
	@echo "Cleaning up memgraph sandbox cluster..."
	@./scripts/clean.sh $(RELEASE_NAME)

status:
	@echo "Checking cluster status..."
	@kubectl get pods -l app.kubernetes.io/name=memgraph
	@kubectl get pvc

logs:
	@echo "Showing recent logs from all memgraph pods..."
	@kubectl logs -l app.kubernetes.io/name=memgraph --tail=50 --since=5m

check:
	@echo "Checking memgraph replication status..."
	@./scripts/check.sh