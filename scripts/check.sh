#!/bin/bash

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
PURPLE='\033[0;35m'
NC='\033[0m' # No Color

# Configuration
NAMESPACE=${MEMGRAPH_NAMESPACE:-"memgraph"}
LABEL_SELECTOR=${MEMGRAPH_LABEL:-"app.kubernetes.io/name=memgraph"}

echo "🔍 Memgraph Cluster Replication Status Check"
echo "=============================================="
echo -e "${PURPLE}📍 Namespace: ${NAMESPACE}${NC}"
echo -e "${PURPLE}🏷️  Label Selector: ${LABEL_SELECTOR}${NC}"

# Function to run a command on a pod and show output
run_memgraph_query() {
    local pod_name=$1
    local query=$2
    local description=$3
    local format=${4:-"table"}  # Default to table format
    
    echo -e "\n${BLUE}📊 ${description} for ${pod_name}:${NC}"
    if [[ "$format" == "csv" ]]; then
        kubectl exec "$pod_name" -n "$NAMESPACE" -c memgraph -- bash -c "echo \"$query\" | mgconsole --username=mguser --password=mgpasswd --output-format csv" 2>/dev/null || echo "❌ Failed to query $pod_name"
    else
        kubectl exec "$pod_name" -n "$NAMESPACE" -c memgraph -- bash -c "echo \"$query\" | mgconsole --username=mguser --password=mgpasswd" 2>/dev/null || echo "❌ Failed to query $pod_name"
    fi
}

# Discover Memgraph pods dynamically
echo -e "\n${YELLOW}🔍 Discovering Memgraph pods...${NC}"
mapfile -t pods < <(kubectl get pods -n "$NAMESPACE" -l "$LABEL_SELECTOR" --no-headers -o custom-columns=":metadata.name" | sort)

if [ ${#pods[@]} -eq 0 ]; then
    echo -e "${RED}❌ No Memgraph pods found in namespace '${NAMESPACE}'${NC}"
    echo -e "${YELLOW}💡 Make sure the cluster is deployed and pods have label '${LABEL_SELECTOR}'${NC}"
    echo -e "${YELLOW}💡 You can override with: MEMGRAPH_NAMESPACE=<namespace> MEMGRAPH_LABEL=<label> $0${NC}"
    exit 1
fi

echo -e "${GREEN}✅ Found ${#pods[@]} Memgraph pod(s): ${pods[*]}${NC}"

# Check if pods exist and show status
echo -e "\n${YELLOW}📋 Pod Status:${NC}"
kubectl get pods -n "$NAMESPACE" -l "$LABEL_SELECTOR" -o wide

# Process each discovered pod
for pod in "${pods[@]}"; do
    # Skip empty pod names
    if [[ -z "$pod" ]]; then
        continue
    fi
    
    # Check if pod is ready
    pod_status=$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
    
    if [[ "$pod_status" != "Running" ]]; then
        echo -e "\n${RED}=== ${pod} (${pod_status}) ===${NC}"
        echo -e "${YELLOW}⚠️  Pod is not running - skipping queries${NC}"
        continue
    fi
    
    echo -e "\n${GREEN}=== ${pod} ===${NC}"
    
    # Check replication role
    run_memgraph_query "$pod" "SHOW REPLICATION ROLE;" "Replication Role"
    
    # Get the role to determine if we should show replicas
    role=$(kubectl exec "$pod" -n "$NAMESPACE" -c memgraph -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole --username=mguser --password=mgpasswd --output-format csv' 2>/dev/null | tail -n +2 | tr -d '"' | tr -d '\r')
    
    if [[ "$role" == "main" ]]; then
        run_memgraph_query "$pod" "SHOW REPLICAS;" "Registered Replicas" "csv"
    fi
    
    # Optionally show storage info (uncomment if needed)
    # run_memgraph_query "$pod" "SHOW STORAGE INFO;" "Storage Info"
done

echo -e "\n${GREEN}✅ Replication status check complete${NC}"
echo -e "${BLUE}📝 Checked ${#pods[@]} pod(s) total${NC}"
