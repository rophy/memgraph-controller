#!/bin/bash
set -euo pipefail

readonly MEMGRAPH_NS="memgraph"
readonly TEST_CLIENT_LABEL="app=neo4j-client"

echo "=== DEBUG E2E TESTS ==="

echo "1. Getting test client pod..."
TEST_CLIENT_POD=$(kubectl get pods -n "$MEMGRAPH_NS" -l "$TEST_CLIENT_LABEL" -o jsonpath='{.items[0].metadata.name}')
echo "Test client pod: $TEST_CLIENT_POD"

echo "2. Testing replication role query..."
kubectl exec "$TEST_CLIENT_POD" -n "$MEMGRAPH_NS" -- node index.js "SHOW REPLICATION ROLE;"

echo "3. Testing replicas query..."
kubectl exec "$TEST_CLIENT_POD" -n "$MEMGRAPH_NS" -- node index.js "SHOW REPLICAS;"

echo "4. Testing simple count query..."
kubectl exec "$TEST_CLIENT_POD" -n "$MEMGRAPH_NS" -- node index.js "MATCH (n) RETURN count(n) as node_count;"

echo "=== ALL TESTS PASSED ==="