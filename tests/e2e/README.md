# E2E Tests

End-to-end tests for the Memgraph Controller that validate cluster behavior, failover scenarios, and data consistency.

## Architecture

Tests run as Kubernetes Jobs in the `memgraph` namespace and connect directly to the `memgraph-controller` service, ensuring they honor readiness probes and only connect to leader pods.

## Test Structure

- `test_*.py` - Individual test cases
- `utils.py` - Shared utilities for cluster monitoring and log parsing
- `requirements.txt` - Python dependencies

## Running Tests

### All Tests
```bash
make test-e2e        # Run all E2E tests
```

### Single Test
```bash
# Run a specific test file
make test-e2e ARGS="test_failover_pod_deletion.py"

# Run a specific test function
make test-e2e ARGS="test_failover_pod_deletion.py::test_pod_deletion_failover"

# Run with additional pytest options
make test-e2e ARGS="-k failover -v"
```

### Reliability Testing
```bash
tests/scripts/repeat-e2e-tests.sh 10  # Run tests N times
```

## Test Client

The test client (`tests/client/client.py`) generates continuous read/write operations using logfmt structured logging:
- Connects to controller service (honors readiness probes)
- Logs operations with timestamps, success/failure status
- Used for validating data consistency during failover scenarios

## Key Features

- **Failover Detection**: Monitors controller logs and database state changes
- **Precondition Validation**: Ensures cluster stability before destructive tests
- **Log Parsing**: Uses logfmt format for structured operation logging
- **Direct Monitoring**: Queries Memgraph pods directly for replication status