# Known Issues

## 1. Gateway Routing Race Condition During Failover

**Status**: Active  
**Severity**: Medium  
**First Observed**: 2025-08-29  
**Occurrence Rate**: ~20% (1 in 5 test runs)

### Description

During failover scenarios, there is a race condition where the gateway may route write queries to a replica instance that was recently demoted from main, resulting in the error:
```
Neo4jError: Memgraph.ClientError.MemgraphError.MemgraphError 
(Write queries are forbidden on the replica instance. Replica instances accept only read queries, 
while the main instance accepts read and write queries. Please retry your query on the main instance.)
```

### Root Cause

The issue occurs due to a timing gap between:
1. **Memgraph role change**: When a main pod is killed, Memgraph quickly promotes a new main and demotes the old main to replica
2. **Gateway routing update**: The controller/gateway takes time to detect and update its routing to the new main
3. **Stale endpoint usage**: During this gap, write queries may be routed to the old main's endpoint, which now serves as a replica

### Reproduction Steps

1. Run E2E tests repeatedly (typically fails after 4-5 successful runs):
   ```bash
   for i in {1..5}; do make test-e2e || break; done
   ```
2. The failure typically occurs during the failover test when:
   - A main pod is deleted
   - The test immediately attempts to write data
   - The write hits the old endpoint before routing is updated

### Evidence

From test run #5:
```
=== Test Run 5/5 ===
TestE2E_FailoverReliability
    failover_test.go:216: 
        Error: Neo4jError: Write queries are forbidden on the replica instance
        Test: TestE2E_FailoverReliability
        Messages: Should write data after failover (with automatic retry)
```

Controller logs show proper detection but potential lag:
```
2025/08/29 22:45:42 Main failover detected - handling failover...
2025/08/29 22:45:42 Gateway: Connection established to main at 10.244.3.85:7687
```

Actual pod roles after failure:
- memgraph-ha-0: main (correct)
- memgraph-ha-1: replica (correct)
- memgraph-ha-2: replica (correct)

### Impact

- **User Experience**: Transient write failures during failover
- **Test Reliability**: E2E tests fail intermittently (~20% failure rate)
- **Recovery**: The system eventually converges to the correct state, but there's a window of incorrect routing

### Workaround

The Neo4j driver's retry mechanism (implemented in commit 99c8a9f) helps but doesn't fully solve this issue because:
- The driver retries against the same endpoint
- The endpoint itself has changed roles (main → replica)
- A different type of retry logic is needed that re-discovers the main endpoint

### Proposed Solutions

1. **Short-term**: Add endpoint re-discovery to the retry logic
   - On "write forbidden on replica" error, force endpoint refresh
   - Query controller for new main endpoint before retry

2. **Medium-term**: Implement proper connection draining
   - Gateway should track Memgraph role changes in real-time
   - Gracefully redirect new connections during role transitions
   - Keep existing connections open but mark them for drainage

3. **Long-term**: Implement Bolt protocol awareness in gateway
   - Parse Bolt protocol messages to detect role changes
   - Inject proper routing table updates to clients
   - Handle connection migration transparently

### Related Components

- **Gateway**: `pkg/gateway/server.go` - Connection routing logic
- **Controller**: `pkg/controller/failover.go` - Failover detection and handling
- **Controller**: `pkg/controller/controller.go:686` - Direct targetMainIndex assignment (potential race)
- **Tests**: `tests/failover_test.go` - Where the issue manifests

### Monitoring

To detect this issue in production:
1. Monitor for "Write queries are forbidden on the replica instance" errors
2. Track gateway routing updates vs actual Memgraph role changes
3. Measure time between failover detection and routing update completion

### Notes

- This is a different issue from the connection EOF errors (fixed in commit 99c8a9f)
- The issue is more likely under rapid successive failovers (stress testing)
- The controller's state synchronization anti-pattern (documented in CLAUDE.md) may contribute to this race condition