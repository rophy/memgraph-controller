# E2E Tests

End-to-end tests for the Memgraph Controller that validate cluster behavior, failover scenarios, and data consistency.

## Architecture

Tests run as Kubernetes Jobs in the `memgraph` namespace and connect directly to the `memgraph-controller` service, ensuring they honor readiness probes and only connect to leader pods.

## Test Structure

- `test_*.py` - Individual test cases
- `utils.py` - Shared utilities for cluster monitoring and log parsing
- `requirements.txt` - Python dependencies

## Running Tests

### Prerequisites
1. Run `make down` to remove skaffold resources
2. Run `make run` to build and deploy memgraph-ha cluster
3. Wait for cluster to stabilize (check memgraph-controller pod logs)

### All Tests
```bash
make test-e2e        # Run all E2E tests
```

### Single Test Execution
The `make test-e2e` command supports the `ARGS` parameter for running specific tests:

```bash
# Run a specific test file
make test-e2e ARGS="tests/e2e/test_rolling_restart.py"

# Run a specific test function  
make test-e2e ARGS="tests/e2e/test_failover_pod_deletion.py::test_pod_deletion_failover"

# Run multiple test patterns
make test-e2e ARGS="-k failover -v"

# Run tests with verbose output
make test-e2e ARGS="tests/e2e/test_rolling_restart.py -v -s"
```

### Reliability Testing
```bash
tests/scripts/repeat-e2e-tests.sh 10  # Run tests N times
```

## Test Output and Debugging

### Log-to-File Design
Tests automatically capture and store logs for debugging:
- **Controller logs**: Saved with timestamps for failover analysis
- **Test client logs**: Continuous operation logs during tests
- **Pod logs**: Individual pod logs captured at key test points

### Log Storage Location
All captured logs are stored in the test execution environment and can be accessed for post-test analysis and issue verification.

### Debugging Failed Tests
When tests fail, examine the captured logs to:
- Verify controller behavior during failover scenarios
- Analyze split-brain conditions during network isolation
- Check reconciliation timing after network recovery
- Validate pod promotion and demotion sequences

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
