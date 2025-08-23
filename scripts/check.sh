#!/bin/bash

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

echo "🔍 Memgraph Cluster Replication Status Check"
echo "=============================================="

# Function to run a command on a pod and show output
run_memgraph_query() {
    local pod_name=$1
    local query=$2
    local description=$3
    
    echo -e "\n${BLUE}📊 ${description} for ${pod_name}:${NC}"
    kubectl exec "$pod_name" -n memgraph -- bash -c "echo \"$query\" | mgconsole" 2>/dev/null || echo "❌ Failed to query $pod_name"
}

# Check if pods exist
echo -e "\n${YELLOW}📋 Pod Status:${NC}"
kubectl get pods -n memgraph -l app.kubernetes.io/name=memgraph -o wide

# For each of the 3 Memgraph pods
pods=(memgraph-ha-0 memgraph-ha-1 memgraph-ha-2)

for pod in "${pods[@]}"; do
    if kubectl get pod "$pod" -n memgraph &>/dev/null; then
        echo -e "\n${GREEN}=== ${pod} ===${NC}"
        
        # Check replication role
        run_memgraph_query "$pod" "SHOW REPLICATION ROLE;" "Replication Role"
        
        # If it's a master, show registered replicas
        role=$(kubectl exec "$pod" -n memgraph -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole' 2>/dev/null | grep -o '"[^"]*"' | tr -d '"')
        
        if [[ "$role" == "main" ]]; then
            run_memgraph_query "$pod" "SHOW REPLICAS;" "Registered Replicas"
        fi
        
        # Show storage info (vertex/edge counts)
        # run_memgraph_query "$pod" "SHOW STORAGE INFO;" "Storage Info"
        
    else
        echo -e "\n${RED}❌ Pod $pod not found${NC}"
    fi
done

echo -e "\n${GREEN}✅ Replication status check complete${NC}"
