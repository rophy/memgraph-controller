#!/bin/bash

set -e

RELEASE_NAME=${1:-memgraph-sandbox}

echo "Setting up Memgraph sandbox with release: $RELEASE_NAME"
echo "Using kubectl active namespace: $(kubectl config view --minify --output 'jsonpath={..namespace}' || echo 'default')"

# Install helm chart
echo "Installing Memgraph helm chart..."
helm upgrade --install "$RELEASE_NAME" . --wait --timeout=10m

# Get pod names
echo "Getting pod names..."
POD_0=$(kubectl get pods -l app.kubernetes.io/name=memgraph -o jsonpath='{.items[0].metadata.name}')
POD_1=$(kubectl get pods -l app.kubernetes.io/name=memgraph -o jsonpath='{.items[1].metadata.name}')
POD_2=$(kubectl get pods -l app.kubernetes.io/name=memgraph -o jsonpath='{.items[2].metadata.name}')

echo "Pods found: $POD_0, $POD_1, $POD_2"

# Setup MAIN-SYNC-ASYNC replication
echo "Setting up MAIN-SYNC-ASYNC cluster..."

# Get pod IPs for replication setup
POD_1_IP=$(kubectl get pod "$POD_1" -o jsonpath='{.status.podIP}')
POD_2_IP=$(kubectl get pod "$POD_2" -o jsonpath='{.status.podIP}')

echo "Pod IPs: $POD_1 -> $POD_1_IP, $POD_2 -> $POD_2_IP"

# Set replica pods to REPLICA role first
echo "Setting $POD_1 to REPLICA role"
kubectl exec -c memgraph "$POD_1" -- bash -c 'echo "SET REPLICATION ROLE TO REPLICA WITH PORT 10000;" | mgconsole --username=memgraph'

echo "Setting $POD_2 to REPLICA role"
kubectl exec -c memgraph "$POD_2" -- bash -c 'echo "SET REPLICATION ROLE TO REPLICA WITH PORT 10000;" | mgconsole --username=memgraph'

# Register replicas on the main pod
echo "Registering $POD_1 as SYNC replica"
kubectl exec -c memgraph "$POD_0" -- bash -c "echo \"REGISTER REPLICA replica1 SYNC TO \\\"$POD_1_IP:10000\\\";\" | mgconsole --username=memgraph"

echo "Registering $POD_2 as ASYNC replica"
kubectl exec -c memgraph "$POD_0" -- bash -c "echo \"REGISTER REPLICA replica2 ASYNC TO \\\"$POD_2_IP:10000\\\";\" | mgconsole --username=memgraph"

# Verify the setup
echo "Verifying replication setup..."
echo "Main pod ($POD_0) role:"
kubectl exec -c memgraph "$POD_0" -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole --output-format csv --username=memgraph'

echo "Registered replicas:"
kubectl exec -c memgraph "$POD_0" -- bash -c 'echo "SHOW REPLICAS;" | mgconsole --output-format csv --username=memgraph'

echo ""
echo "✅ Memgraph MAIN-SYNC-ASYNC cluster is ready!"
echo "   Main: $POD_0"
echo "   Sync Replica: $POD_1 ($POD_1_IP)"
echo "   Async Replica: $POD_2 ($POD_2_IP)"
echo ""
echo "Use 'make status' to check cluster status"
echo "Use 'make logs' to view recent logs"