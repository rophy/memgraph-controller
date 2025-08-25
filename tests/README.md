# E2E Tests for Memgraph Controller with Gateway

This directory contains end-to-end tests that validate the complete functionality of the memgraph-controller with its integrated gateway.

## Prerequisites

1. **Deploy the cluster**: Run `make run` from the project root to deploy the full cluster with controller
2. **Port forwarding**: Skaffold should be forwarding ports:
   - `7687` → memgraph-controller service (gateway)  
   - `8080` → memgraph-controller service (status API)

## Test Structure

### TestE2E_ClusterTopology
- **Purpose**: Validates the cluster has the expected main-sync-async topology
- **Checks**:
  - 3 pods are present and healthy
  - memgraph-0 is the main (main role)
  - memgraph-1 is the sync replica  
  - memgraph-2 is the async replica
  - Gateway points to the correct main

### TestE2E_DataWriteThoughGateway
- **Purpose**: Writes test data through the controller gateway
- **Checks**:
  - Connection works through port 7687 (gateway)
  - Data can be written via Cypher queries
  - Written data can be read back correctly
  - Sets environment variables for the replication test

### TestE2E_DataReplicationVerification  
- **Purpose**: Verifies data replication across all 3 pods
- **Checks**:
  - Connects directly to each pod's bolt address
  - Verifies test data from previous test exists on all pods
  - Confirms main-sync-async replication is working

## Running the Tests

```bash
# From the tests directory
cd tests/
go mod tidy
go test -v ./...

# Run a specific test
go test -v -run TestE2E_ClusterTopology

# Run with timeout
go test -v -timeout 2m ./...
```

## Test Flow

1. **Deploy**: `make run` (deploys cluster + controller with gateway)
2. **Wait**: Tests wait for controller to be ready on port 8080
3. **Validate**: Check cluster topology via status API
4. **Write**: Write test data through gateway on port 7687
5. **Replicate**: Wait for replication to complete
6. **Verify**: Check all pods have the test data

## Expected Output

```
=== RUN   TestE2E_ClusterTopology
    e2e_test.go:89: ✓ Cluster topology validated: Main=memgraph-0, Sync=memgraph-1, Total pods=3
--- PASS: TestE2E_ClusterTopology (2.34s)
=== RUN   TestE2E_DataWriteThoughGateway  
    e2e_test.go:134: ✓ Data write validated: ID=test_1703123456, Value=value_1703123456
--- PASS: TestE2E_DataWriteThoughGateway (1.23s)
=== RUN   TestE2E_DataReplicationVerification
    e2e_test.go:185: ✓ Data found on pod memgraph-0 (main)
    e2e_test.go:185: ✓ Data found on pod memgraph-1 (replica)
    e2e_test.go:185: ✓ Data found on pod memgraph-2 (replica)
    e2e_test.go:195: ✓ Data replication validated: 3/3 pods have the test data
--- PASS: TestE2E_DataReplicationVerification (3.45s)
```

## Troubleshooting

- **Connection refused on 8080**: Controller not ready, wait longer
- **Connection refused on 7687**: Gateway not enabled or not forwarded  
- **Data not replicated**: Check pod health, replication may take time
- **Pod connection failed**: Check pod bolt addresses in status API