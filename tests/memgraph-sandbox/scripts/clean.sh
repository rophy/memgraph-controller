#!/bin/bash

set -e

CURRENT_NAMESPACE=$(kubectl config view --minify --output 'jsonpath={..namespace}' || echo 'default')

echo "Cleaning up Memgraph sandbox with skaffold"
echo "Using kubectl active namespace: $CURRENT_NAMESPACE"

# Clean up with skaffold
echo "Deleting skaffold deployments..."
skaffold delete

# Wait for pods to be deleted
echo "Waiting for pods to be deleted..."
kubectl wait --for=delete pod -l app.kubernetes.io/name=memgraph --timeout=60s || true

# Delete PVCs
echo "Deleting PVCs..."
PVC_COUNT=$(kubectl get pvc -o name 2>/dev/null | wc -l)
if [ "$PVC_COUNT" -gt 0 ]; then
    kubectl delete pvc --all
    echo "Deleted $PVC_COUNT PVCs"
else
    echo "No PVCs found to delete"
fi

echo "✅ Cleanup completed successfully!"