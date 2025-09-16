# Memgraph WebSocket Logs Documentation

## Overview

Memgraph provides real-time log streaming via WebSocket on port 7444. All logs are sent in JSON format and provide comprehensive visibility into database operations, replication events, and system state changes.

## WebSocket Connection

- **Endpoint**: `ws://<pod-ip>:7444`
- **Protocol**: WebSocket (RFC 6455)
- **Format**: Each message is a JSON object

## Message Format

All WebSocket messages follow this JSON structure:

```json
{
  "event": "log",
  "level": "<LOG_LEVEL>",
  "message": "<LOG_MESSAGE>"
}
```

Where:
- `event`: Always "log" for log messages
- `level`: One of TRACE, DEBUG, INFO, WARNING, ERROR, CRITICAL
- `message`: The actual log content

## Log Level Configuration

### Default Log Level
- **Default**: WARNING
- **Result**: Only WARNING, ERROR, and CRITICAL messages are sent

### Changing Log Level
```sql
-- Set to TRACE for maximum visibility
SET DATABASE SETTING "log.level" TO "TRACE";

-- Set to INFO for moderate visibility
SET DATABASE SETTING "log.level" TO "INFO";

-- Set to WARNING (default)
SET DATABASE SETTING "log.level" TO "WARNING";
```

### Verification
```sql
SHOW DATABASE SETTINGS;
```

## Log Levels and Content

### TRACE Level
**Most verbose level** - includes all system operations

```json
{"event": "log", "level": "TRACE", "message": "Executing query: SHOW REPLICAS;"}
{"event": "log", "level": "TRACE", "message": "Starting transaction"}
{"event": "log", "level": "TRACE", "message": "Commit transaction"}
{"event": "log", "level": "TRACE", "message": "Query executed successfully"}
{"event": "log", "level": "TRACE", "message": "Connection established from 10.96.0.1:44556"}
```

**Key Operations Visible at TRACE:**
- Individual query execution
- Transaction boundaries
- Connection management
- Internal function calls
- Memory operations
- Lock acquisitions

### DEBUG Level
**Development and troubleshooting information**

```json
{"event": "log", "level": "DEBUG", "message": "Replica registration attempt from memgraph-ha-1.memgraph-ha"}
{"event": "log", "level": "DEBUG", "message": "Replication lag check: 0ms"}
{"event": "log", "level": "DEBUG", "message": "Snapshot creation initiated"}
{"event": "log", "level": "DEBUG", "message": "Storage recovery completed"}
```

**Key Operations Visible at DEBUG:**
- Replication status checks
- Performance metrics
- Storage operations
- Recovery procedures
- Configuration changes

### INFO Level
**General operational information**

```json
{"event": "log", "level": "INFO", "message": "Replica memgraph-ha-1.memgraph-ha:7687 registered successfully"}
{"event": "log", "level": "INFO", "message": "Role changed to MAIN"}
{"event": "log", "level": "INFO", "message": "Role changed to REPLICA"}
{"event": "log", "level": "INFO", "message": "Snapshot created successfully"}
{"event": "log", "level": "INFO", "message": "Database started successfully"}
```

**Key Operations Visible at INFO:**
- Role changes (MAIN ↔ REPLICA)
- Replica registration/deregistration
- Snapshot operations
- Database lifecycle events
- Successful configuration changes

### WARNING Level (Default)
**Potential issues that don't stop operation**

```json
{"event": "log", "level": "WARNING", "message": "Replica memgraph-ha-2.memgraph-ha:7687 is not responding"}
{"event": "log", "level": "WARNING", "message": "Replication lag is high: 1500ms"}
{"event": "log", "level": "WARNING", "message": "Connection timeout from client"}
{"event": "log", "level": "WARNING", "message": "Query execution took longer than expected: 5.2s"}
```

**Key Operations Visible at WARNING:**
- Replica connectivity issues
- Performance warnings
- Timeout warnings
- Resource usage alerts

### ERROR Level
**Errors that affect functionality**

```json
{"event": "log", "level": "ERROR", "message": "Failed to register replica memgraph-ha-1.memgraph-ha:7687"}
{"event": "log", "level": "ERROR", "message": "Replication connection lost"}
{"event": "log", "level": "ERROR", "message": "Query execution failed: Syntax error"}
{"event": "log", "level": "ERROR", "message": "Storage corruption detected"}
```

**Key Operations Visible at ERROR:**
- Replication failures
- Query execution errors
- Storage issues
- Network connectivity problems

### CRITICAL Level
**Critical system failures**

```json
{"event": "log", "level": "CRITICAL", "message": "Database shutdown due to fatal error"}
{"event": "log", "level": "CRITICAL", "message": "Unrecoverable storage corruption"}
{"event": "log", "level": "CRITICAL", "message": "Out of memory - terminating"}
```

**Key Operations Visible at CRITICAL:**
- System shutdowns
- Unrecoverable errors
- Memory exhaustion
- Fatal storage problems

## State Change Detection Patterns

### Role Changes
```json
{"event": "log", "level": "INFO", "message": "Role changed to MAIN"}
{"event": "log", "level": "INFO", "message": "Role changed to REPLICA"}
```

**Detection Pattern**: `Role changed to (MAIN|REPLICA)`

### Replica Management

#### Main Node Logs
When the main node registers or manages replicas, it produces logs:

```json
{"event": "log", "level": "DEBUG", "message": "REGISTER REPLICA memgraph_ha_1 SYNC TO \"memgraph-ha-1.memgraph-ha:10000\""}
{"event": "log", "level": "TRACE", "message": "Current timestamp on replica memgraph_ha_1 for db memgraph is 0"}
{"event": "log", "level": "TRACE", "message": "Closing replication client on 10.244.120.77:10000"}
{"event": "log", "level": "INFO", "message": "Replica memgraph-ha-1.memgraph-ha:7687 registered successfully"}
{"event": "log", "level": "INFO", "message": "Replica memgraph-ha-2.memgraph-ha:7687 dropped"}
{"event": "log", "level": "ERROR", "message": "Failed to register replica memgraph-ha-1.memgraph-ha:7687"}
```

#### Replica Node Logs  
**Important**: Replicas do NOT produce logs when being registered by the main node. The registration is a main-initiated operation, and replicas passively accept the connection without logging.

#### Replication Activity and State Logs
When data is replicated, only the main node logs the activity and replica states:

```json
{"event": "log", "level": "TRACE", "message": "Starting transaction replication for replica memgraph_ha_1 in state READY"}
{"event": "log", "level": "TRACE", "message": "Finalizing transaction on replica memgraph_ha_1 in state REPLICATING"}
```

**Replica States in Logs**:
- **READY**: Replica is ready to receive new transactions
- **REPLICATING**: Replica is actively receiving/processing a transaction
- **INVALID**: Replica is in an error state (appears when replication fails)

The state transitions are visible at TRACE level:
1. Replica starts in `READY` state
2. When transaction begins, logs show "Starting transaction replication... in state READY"
3. During replication, state changes to `REPLICATING`
4. When complete, logs show "Finalizing transaction... in state REPLICATING"
5. Replica returns to `READY` state for next transaction

**Detection Patterns**: 
- Registration (main only): `REGISTER REPLICA|Replica .* registered successfully`
- Deregistration (main only): `DROP REPLICA|Replica .* dropped`
- Failure (main only): `Failed to register replica .*`
- Replication activity (main only): `Starting transaction replication|Finalizing transaction`

### Replication Issues
```json
{"event": "log", "level": "WARNING", "message": "Replica memgraph-ha-2.memgraph-ha:7687 is not responding"}
{"event": "log", "level": "ERROR", "message": "Replication connection lost"}
{"event": "log", "level": "WARNING", "message": "Replication lag is high: 1500ms"}
```

**Detection Patterns**:
- Connectivity: `Replica .* is not responding`
- Connection loss: `Replication connection lost`
- Lag warning: `Replication lag is high: \d+ms`

### Database Lifecycle
```json
{"event": "log", "level": "INFO", "message": "Database started successfully"}
{"event": "log", "level": "INFO", "message": "Database shutdown initiated"}
{"event": "log", "level": "CRITICAL", "message": "Database shutdown due to fatal error"}
```

**Detection Patterns**:
- Startup: `Database started successfully`
- Graceful shutdown: `Database shutdown initiated`
- Fatal shutdown: `Database shutdown due to fatal error`

### Snapshot Operations
```json
{"event": "log", "level": "INFO", "message": "Snapshot created successfully"}
{"event": "log", "level": "DEBUG", "message": "Snapshot creation initiated"}
{"event": "log", "level": "ERROR", "message": "Snapshot creation failed"}
```

**Detection Patterns**:
- Success: `Snapshot created successfully`
- Initiation: `Snapshot creation initiated`
- Failure: `Snapshot creation failed`

## Usage Recommendations

### For Production Monitoring
- **Recommended Level**: INFO or WARNING
- **Focus**: Role changes, replica status, major errors
- **Performance**: Lower message volume, efficient processing

### For Development/Debugging
- **Recommended Level**: TRACE or DEBUG  
- **Focus**: Detailed operation flow, performance metrics
- **Performance**: High message volume, requires filtering

### For Troubleshooting
- **Recommended Level**: DEBUG or TRACE
- **Duration**: Temporary (change back after investigation)
- **Focus**: Specific error reproduction and analysis

## Performance Considerations

### Message Volume by Level
- **CRITICAL**: ~1-5 messages per day
- **ERROR**: ~10-50 messages per day
- **WARNING**: ~50-200 messages per day
- **INFO**: ~500-2000 messages per day
- **DEBUG**: ~5,000-20,000 messages per day
- **TRACE**: ~50,000+ messages per day

### WebSocket Client Recommendations
- Use message buffering for high-volume levels
- Implement reconnection logic for connection failures
- Filter messages client-side for specific patterns
- Consider log rotation for persistent storage

## Implementation Notes

### Connection Management
```python
import websocket
import json

def on_message(ws, message):
    try:
        log_entry = json.loads(message)
        level = log_entry.get("level")
        msg = log_entry.get("message")
        print(f"[{level}] {msg}")
    except json.JSONDecodeError:
        print(f"Invalid JSON: {message}")

ws = websocket.WebSocketApp(
    "ws://memgraph-ha-0.memgraph-ha:7444",
    on_message=on_message
)
ws.run_forever()
```

### Error Handling
- Always parse JSON before processing
- Handle WebSocket disconnections gracefully
- Implement exponential backoff for reconnections
- Log WebSocket client errors separately

### Message Filtering
```python
def filter_important_messages(log_entry):
    level = log_entry.get("level")
    message = log_entry.get("message", "")
    
    # Critical system events
    if level in ["ERROR", "CRITICAL"]:
        return True
        
    # Role changes
    if "Role changed to" in message:
        return True
        
    # Replica events
    if any(keyword in message for keyword in [
        "registered successfully", "dropped", "not responding"
    ]):
        return True
        
    return False
```

## Comparison: Pod Logs vs WebSocket Logs

### Key Differences

| Aspect | Pod Logs (`kubectl logs`) | WebSocket Logs (port 7444) |
|--------|---------------------------|----------------------------|
| **Startup Logs** | ✅ Includes all startup warnings and initialization | ❌ Misses startup logs (before connection) |
| **Format** | Plain text with timestamps | JSON format |
| **Real-time** | ✅ All logs from pod start | ✅ All logs after WebSocket connects |
| **Log Level** | Respects SET DATABASE SETTING | Respects SET DATABASE SETTING |
| **Content** | Complete log history | Same content as pod logs (after connection) |

### Example Comparison

**Same query in both formats:**

Pod logs:
```
2025-09-14T20:03:00.578Z [2025-09-14 20:03:00.578] [memgraph_log] [debug] [Run - memgraph] 'CREATE (n:Test {id: 1})'
```

WebSocket logs:
```json
{"event": "log", "level": "debug", "message": "[Run - memgraph] 'CREATE (n:Test {id: 1})'"}
```

### Important Notes

1. **Both respect log level settings** - When you set `log.level` to TRACE, both pod logs and WebSocket will show TRACE level messages
2. **WebSocket only captures logs after connection** - Any logs generated before the WebSocket connection (like startup warnings) are not available via WebSocket
3. **Same log content** - The actual log messages are identical; only the format differs
4. **Use pod logs for historical data** - If you need startup logs or logs from before WebSocket connection
5. **Use WebSocket for real-time monitoring** - Better for programmatic consumption due to JSON format

## Sync Replica Failure and Write Operations

### Memgraph Replication Modes

Memgraph supports two replication modes in the REGISTER REPLICA command:

1. **SYNC mode**: Main blocks writes if sync replica cannot confirm transaction
2. **ASYNC mode**: Main doesn't wait for replica acknowledgment

### WebSocket Logs During Normal Replication

When sync replicas are healthy:

```json
{"event": "log", "level": "trace", "message": "Starting transaction replication for replica replica1 in state READY"}
{"event": "log", "level": "trace", "message": "Finalizing transaction on replica replica1 in state REPLICATING"}
```

### WebSocket Logs When Sync Replica Fails

When sync replicas are in invalid/failed state and writes are attempted:

```json
{"event": "log", "level": "trace", "message": "Starting transaction replication for replica replica1 in state MAYBE_BEHIND"}
{"event": "log", "level": "error", "message": "Couldn't replicate data to replica1. For more details, visit https://memgr.ph/replication."}
{"event": "log", "level": "trace", "message": "Finalizing transaction on replica replica1 in state MAYBE_BEHIND"}
{"event": "log", "level": "trace", "message": "Error message: At least one SYNC replica has not confirmed committing last transaction."}
{"event": "log", "level": "debug", "message": "Replica 'replica1' can't respond or missing database 'memgraph' - 'dc079eba-1874-498f-96a1-5862e02de6eb'"}
```

### Key Error Patterns

1. **Replica State**: `MAYBE_BEHIND` indicates the replica is not synchronized
2. **Error Level Logs**: `Couldn't replicate data to replica1`
3. **Transaction Rejection**: `At least one SYNC replica has not confirmed committing last transaction`
4. **Debug Messages**: `Replica 'replica1' can't respond or missing database`

### Client Error Response

When sync replica fails, the client receives:
```
Client received query exception: At least one SYNC replica has not confirmed committing last transaction.
```

### Important Behavior

1. **Write Blocking**: With SYNC replicas in invalid state, the main node **rejects writes** immediately
2. **No Timeout Wait**: The write fails immediately without waiting for 10s RPC timeout
3. **Error Visibility**: Both pod logs and WebSocket logs show clear error messages
4. **State Tracking**: Replica state changes from `READY` → `MAYBE_BEHIND` when issues occur

**Conclusion**: Memgraph properly blocks writes when SYNC replicas cannot confirm transactions, and these errors ARE visible in WebSocket logs at error, trace, and debug levels.

### Replica-Side Logs During MAYBE_BEHIND State

When replicas are in MAYBE_BEHIND state (from the main's perspective), the **replica pods themselves** generate the following logs:

#### Warning Level Messages (Repeated Every Second)
```json
{"event": "log", "level": "warning", "message": "No database with UUID \"dc079eba-1874-498f-96a1-5862e02de6eb\" on replica!"}
{"event": "log", "level": "warning", "message": "No database accessor"}
```

#### Debug Level Messages
```json
{"event": "log", "level": "debug", "message": "Aligning database with name memgraph which has UUID 2fc4ebab-4a19-4e89-a682-89b3ba97700b, where config UUID is dc079eba-1874-498f-96a1-5862e02de6eb"}
{"event": "log", "level": "debug", "message": "Different UUIDs"}
{"event": "log", "level": "debug", "message": "Default storage is not clean, cannot update UUID..."}
```

#### Trace Level Messages
```json
{"event": "log", "level": "trace", "message": "[RpcServer] received HeartbeatReq"}
{"event": "log", "level": "trace", "message": "[RpcServer] sent HeartbeatRes"}
{"event": "log", "level": "trace", "message": "[RpcServer] received SystemRecoveryReq"}
{"event": "log", "level": "trace", "message": "[RpcServer] sent SystemRecoveryRes"}
```

#### Key Points About Replica Logs

1. **Replicas do NOT log "MAYBE_BEHIND" explicitly** - this state name only appears in main node logs
2. **UUID mismatch is the root cause** - Replicas repeatedly log database UUID mismatches
3. **Heartbeat continues** - Replicas still respond to heartbeats from main despite UUID mismatch
4. **Frequency** - Warning messages appear approximately once per second per replica
5. **No transaction logs** - Replicas don't log rejected transactions since main blocks them before sending

## Security Considerations

- WebSocket port 7444 provides full log access
- No authentication required on WebSocket endpoint
- Logs may contain sensitive query information at TRACE level
- Restrict network access to WebSocket port in production
- Consider log sanitization for external monitoring systems