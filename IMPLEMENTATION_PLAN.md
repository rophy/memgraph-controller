# E2E Test Python Migration Implementation Plan

## Overview
Migrate existing shell-based e2e tests to Python for better maintainability, JSON handling, and debugging capabilities while keeping the simple kubectl exec approach.

## Stage 1: Python Infrastructure Setup
**Goal**: Create basic Python test infrastructure with utilities
**Success Criteria**: 
- Python utils module with kubectl/memgraph helpers
- Basic test runner working
- Same functionality as shell scripts
**Tests**: Run simple topology test in Python
**Status**: Not Started

### Tasks:
1. Create `tests/e2e/` directory structure
2. Implement `utils.py` with core functions:
   - `kubectl_exec()` - wrapper for kubectl exec commands
   - `get_test_client_pod()` - find test client pod
   - `memgraph_query()` - execute Cypher queries via test client
   - `wait_for_cluster_ready()` - cluster convergence logic
   - Basic logging/output functions
3. Create `conftest.py` for pytest configuration
4. Implement first basic test: `test_basic.py` with cluster topology test
5. Update Makefile with Python test targets

## Makefile Integration:
- `make test-e2e-python` - run Python e2e tests  
- `make test-e2e` - could eventually point to Python version

## Stage 2: Core Test Migration
**Goal**: Migrate main functional tests from shell to Python
**Success Criteria**: 
- All core tests (topology, data write/read, replication) working in Python
- Test results match shell script behavior
- Better error messages and debugging info
**Tests**: Run migrated tests against live cluster
**Status**: Not Started

### Tasks:
1. Migrate `test_cluster_topology()` from shell script
2. Migrate `test_data_write_gateway()` 
3. Migrate `test_data_replication()`
4. Add utilities for:
   - JSON response parsing and validation
   - Test data generation with unique IDs
   - Node counting and verification
5. Verify all tests pass consistently

## Stage 3: Advanced Test Scenarios
**Goal**: Migrate complex failover and restart tests
**Success Criteria**: 
- Network partition failover test working
- Rolling restart test working
- NetworkPolicy management in Python
**Tests**: Run full failover scenarios
**Status**: Not Started

### Tasks:
1. Implement NetworkPolicy helpers:
   - `create_network_isolation()`
   - `remove_network_isolation()`
   - `call_admin_reset_connections()`
2. Migrate `test_network_partition_failover()`
3. Migrate `test_rolling_restart()`
4. Add utilities for:
   - Waiting for write failures/recovery
   - Pod identification and status checking
   - StatefulSet generation tracking

## Stage 4: Test Infrastructure & CI Integration
**Goal**: Complete test infrastructure with proper CI integration
**Success Criteria**: 
- Python tests integrated into Makefile
- Proper test reporting and logging
- Error handling and cleanup
**Tests**: Run tests via make commands
**Status**: Not Started

### Tasks:
1. Update Makefile targets:
   - `make test-e2e-python` - run Python e2e tests
   - `make test-e2e-cleanup-python` - cleanup after Python tests
2. Implement test repeat functionality (equivalent to `repeat-e2e-tests.sh`)
3. Add proper test reporting and log collection
4. Update documentation in CLAUDE.md
5. Consider deprecation path for shell scripts

## Stage 5: Validation & Documentation
**Goal**: Ensure Python tests are production-ready replacement
**Success Criteria**: 
- Python tests pass consistently (same reliability as shell)
- Documentation updated
- Team can use Python tests effectively
**Tests**: Run both shell and Python tests in parallel for validation
**Status**: Not Started

### Tasks:
1. Run comparative testing (shell vs Python) for reliability
2. Update CLAUDE.md with Python test instructions
3. Create migration guide for developers
4. Performance comparison and optimization if needed
5. Plan deprecation of shell scripts once Python tests proven stable

## File Structure:
```
tests/
├── e2e/
│   ├── conftest.py              # pytest configuration
│   ├── utils.py                 # kubectl/memgraph utilities
│   ├── test_basic.py            # topology, data write/read tests
│   ├── test_failover.py         # failover scenarios
│   ├── test_restart.py          # rolling restart tests
│   └── requirements.txt         # pytest and dependencies
├── scripts/                     # keep existing shell scripts during transition
│   ├── run-e2e-tests.sh        # existing shell tests
│   └── ...
```

## Dependencies:
- Python 3.8+
- pytest
- kubectl (existing requirement)
- jq (can be eliminated in Python version)

## Risk Mitigation:
- Keep shell scripts during transition period
- Gradual migration with validation at each stage  
- Comparative testing to ensure no regression
- Easy rollback to shell if issues discovered