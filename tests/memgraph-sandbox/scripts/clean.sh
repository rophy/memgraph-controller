#!/bin/bash

set -e

RELEASE_NAME=${1:-memgraph-sandbox}
CURRENT_NAMESPACE=$(kubectl config view --minify --output 'jsonpath={..namespace}' || echo 'default')

echo "Cleaning up Memgraph sandbox with release: $RELEASE_NAME"
echo "Using kubectl active namespace: $CURRENT_NAMESPACE"

# Check if helm release exists
if helm list | grep -q "$RELEASE_NAME"; then
    echo "Uninstalling helm release: $RELEASE_NAME"
    helm uninstall "$RELEASE_NAME"
else
    echo "Helm release $RELEASE_NAME not found in current namespace"
fi

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