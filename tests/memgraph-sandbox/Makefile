.PHONY: init clean status logs check pause-client resume-client help

help:
	@echo "Memgraph Sandbox - Available Commands:"
	@echo ""
	@echo "  make init          - Deploy memgraph cluster and test client with skaffold"
	@echo "  make clean         - Remove all deployments and PVCs"
	@echo "  make status        - Show pod and PVC status"
	@echo "  make logs          - Show recent logs from memgraph pods"
	@echo "  make check         - Show detailed replication status"
	@echo "  make pause-client  - Pause test client data sending (SIGUSR1)"
	@echo "  make resume-client - Resume test client data sending (SIGUSR2)"
	@echo "  make help          - Show this help message"
	@echo ""
	@echo "Test client signals:"
	@echo "  SIGUSR1 - Pause data sending"
	@echo "  SIGUSR2 - Resume data sending"
	@echo "  SIGTERM/SIGINT - Graceful shutdown"

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

pause-client:
	@echo "Pausing test client..."
	@kubectl exec deployments/test-client -- kill -USR1 1

resume-client:
	@echo "Resuming test client..."
	@kubectl exec deployments/test-client -- kill -USR2 1