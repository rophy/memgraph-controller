.PHONY: init clean status logs check

init:
	@echo "Initializing memgraph sandbox cluster..."
	@./scripts/init.sh

clean:
	@echo "Cleaning up memgraph sandbox cluster..."
	@./scripts/clean.sh

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