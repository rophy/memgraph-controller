# Memgraph Controller Robust Failover Implementation Plan

## Overview

This plan implements a simplified robust master selection and failover strategy to fix critical flaws in the current controller design. The approach uses a clean two-phase bootstrap/operational model with minimal complexity.

## Critical Design Flaws in Current Implementation

### ❌ **Unsafe Bootstrap Behavior**
- **Current**: Processes MIXED states (some main, some replica) without safety checks
- **Risk**: May promote wrong pod, causing data divergence or loss
- **Required**: Refuse to start on ambiguous states during bootstrap

### ❌ **No Controller State Tracking**
- **Current**: No persistent tracking of which pod should be master
- **Risk**: Split-brain scenarios not properly resolved
- **Required**: Remember target master and enforce it consistently

### ❌ **Flawed Master Selection Priority**
- **Current**: Timestamp-based fallback can override SYNC replicas
- **Risk**: ASYNC replicas promoted over SYNC replicas, causing data loss
- **Required**: Strict priority: existing MAIN > SYNC replica > REFUSE

## Simplified Two-Phase Approach

### **Phase 1: Bootstrap (Discover & Remember)**
- Discover cluster state by querying ALL pods
- Apply deterministic rules to choose target master
- Remember the decision in controller memory
- Fail if cluster state is ambiguous

### **Phase 2: Operational (Track & Enforce)**
- Track target master as simple index (0 or 1)
- Enforce target topology during reconciliation
- Handle failover by swapping target index
- Maintain SYNC replica strategy for consistency

## Configuration

### **StatefulSet Name Configuration**

The controller now supports configurable StatefulSet names via the `STATEFULSET_NAME` environment variable:

```bash
# Example: Managing memgraph deployed as "my-db-memgraph"
export STATEFULSET_NAME="my-db-memgraph"

# Pod names will be: my-db-memgraph-0, my-db-memgraph-1, my-db-memgraph-2, etc.
# Eligible pods for master/SYNC: my-db-memgraph-0 and my-db-memgraph-1
# Other pods (my-db-memgraph-2+) are always ASYNC replicas
```

### **Helm Chart Configuration**

```yaml
# values.yaml
env:
  STATEFULSET_NAME: "my-release-memgraph"  # Set to match your Memgraph StatefulSet name
  
# Or use default "memgraph" if not specified
```

### **Usage Examples**

```bash
# Default deployment (manages "memgraph" StatefulSet)
helm install memgraph-controller ./charts/memgraph-controller

# Custom StatefulSet name
helm install memgraph-controller ./charts/memgraph-controller \
  --set env.STATEFULSET_NAME="my-production-memgraph"
```

## Reconciliation Design Patterns

### **When to Reconcile**

The controller uses three reconciliation patterns for robust operation:

#### **1. Event-Driven Reconciliation (Primary)**
Triggered when actual state differs from desired state:

```go
// Pod state changes that require reconciliation
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
    oldPod := oldObj.(*v1.Pod)
    newPod := newObj.(*v1.Pod)
    
    // Only reconcile for meaningful changes
    if oldPod.Status.Phase != newPod.Status.Phase ||
       oldPod.Status.PodIP != newPod.Status.PodIP ||
       oldPod.DeletionTimestamp != newPod.DeletionTimestamp {
        c.enqueueReconciliation("pod-state-changed")
    }
}

func (c *MemgraphController) onPodDelete(obj interface{}) {
    // Pod deletion may require master failover
    c.enqueueReconciliation("pod-deleted")
}
```

**Event Triggers:**
- Pod lifecycle changes (Pending → Running → Failed)
- Pod IP address changes (network reassignment)
- Pod deletion (potential master failure)
- StatefulSet scaling events

#### **2. Periodic Reconciliation (Safety Net)**
Ensures system remains in desired state despite missed events:

```go
func (c *MemgraphController) Run(ctx context.Context) error {
    // Event-driven + periodic reconciliation
    ticker := time.NewTicker(c.config.ReconcileInterval) // Default: 30s
    
    for {
        select {
        case <-ticker.C:
            c.enqueueReconciliation("periodic-safety-check")
        case request := <-c.workQueue:
            c.processReconcileRequest(ctx, request)
        }
    }
}
```

**Why Periodic is Essential:**
- **Missed events** - Network issues causing event loss
- **External changes** - Manual Memgraph role modifications
- **Configuration drift** - Gradual deviation from desired state
- **Split-brain recovery** - Network partition resolution

#### **3. On-Demand Reconciliation**
User-initiated or startup reconciliation:

```go
func (c *MemgraphController) Bootstrap(ctx context.Context) error {
    // Always reconcile on controller startup
    c.enqueueReconciliation("controller-startup")
    return c.reconcileWithBootstrap(ctx)
}

// Future: API endpoint for manual reconciliation
func (c *MemgraphController) handleForceReconcile(w http.ResponseWriter, r *http.Request) {
    c.enqueueReconciliation("user-requested")
}
```

### **Memgraph-Specific Reconciliation Triggers**

#### **Master Failure Scenarios:**
```
Event: Pod-0 (master) becomes unavailable
Trigger: onPodUpdate (Phase: Running → Failed)
Action: Promote pod-1 (SYNC replica) to master
Result: Swap target master index (0 → 1)
```

#### **Split-Brain Detection:**
```
Event: Network partition recovery
Trigger: Periodic reconciliation discovers multiple masters
Action: Apply lower-index precedence rule
Result: Demote higher-index master, restore replication
```

#### **Configuration Drift:**
```
Event: Someone manually changes Memgraph roles
Trigger: Periodic reconciliation
Action: Restore expected topology based on target master index
Result: Re-configure replication to match controller state
```

### **Reconciliation Best Practices**

#### **1. Idempotent Operations**
```go
func (c *MemgraphController) Reconcile(ctx context.Context) error {
    // Safe to run multiple times - only make necessary changes
    targetIndex, isBootstrapped := c.controllerState.GetTargetMaster()
    if !isBootstrapped {
        return c.Bootstrap(ctx)
    }
    
    currentState := c.discoverCurrentState(ctx)
    expectedState := c.calculateExpectedState(targetIndex)
    
    if currentState.matches(expectedState) {
        return nil // No action needed
    }
    
    return c.enforceExpectedState(ctx, currentState, expectedState)
}
```

#### **2. Work Queue with Rate Limiting**
```go
func (c *MemgraphController) processReconcileRequest(ctx context.Context, request reconcileRequest) {
    // Exponential backoff for failed reconciliations
    err := c.reconcileWithBackoff(ctx)
    
    if err != nil {
        c.failureCount++
        if c.failureCount >= c.maxFailures {
            log.Printf("Maximum failures reached - requiring manual intervention")
        }
    } else {
        c.failureCount = 0 // Reset on success
    }
}
```

#### **3. Smart Event Filtering**
```go
func (c *MemgraphController) shouldReconcile(oldPod, newPod *v1.Pod) bool {
    // Avoid unnecessary reconciliation
    if oldPod.ResourceVersion == newPod.ResourceVersion {
        return false // No actual change
    }
    
    // Only reconcile for state changes that affect Memgraph topology
    meaningfulChanges := []bool{
        oldPod.Status.Phase != newPod.Status.Phase,           // Lifecycle change
        oldPod.Status.PodIP != newPod.Status.PodIP,           // Network change
        oldPod.DeletionTimestamp != newPod.DeletionTimestamp, // Deletion started
        oldPod.Spec.NodeName != newPod.Spec.NodeName,         // Node migration
    }
    
    for _, hasChange := range meaningfulChanges {
        if hasChange {
            return true
        }
    }
    
    return false // Ignore cosmetic changes
}
```

### **Anti-Patterns to Avoid**

#### **Over-Reconciliation:**
```go
// ❌ DON'T reconcile on every event
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
    c.enqueueReconciliation("any-change") // Too aggressive
}

// ✅ DO filter for meaningful changes only
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
    if c.shouldReconcile(oldPod, newPod) {
        c.enqueueReconciliation("meaningful-change")
    }
}
```

#### **Blocking Event Handlers:**
```go
// ❌ DON'T perform heavy work in event handlers
func (c *MemgraphController) onPodDelete(obj interface{}) {
    c.Reconcile(context.TODO()) // Blocks Kubernetes informer
}

// ✅ DO enqueue work for background processing
func (c *MemgraphController) onPodDelete(obj interface{}) {
    c.enqueueReconciliation("pod-deleted") // Non-blocking
}
```

## Implementation Stages

---

## **Stage 1: Controller State Tracking**

**Goal**: Add simple state tracking to remember target master decisions
**Success Criteria**: Controller remembers which pod should be master and enforces it consistently
**Tests**: State persistence and failover tests

### Implementation Tasks

#### 1.1 Add Controller State Structure
```go
// pkg/controller/controller_state.go
type ControllerState struct {
    targetMasterIndex int  // 0 or 1 - which pod should be master (${STATEFULSET_NAME}-0 or ${STATEFULSET_NAME}-1)
    isBootstrapped    bool // whether initial bootstrap has completed
    mu                sync.RWMutex
}

func (cs *ControllerState) SetTargetMaster(index int) {
    cs.mu.Lock()
    defer cs.mu.Unlock()
    cs.targetMasterIndex = index
    cs.isBootstrapped = true
}

func (cs *ControllerState) GetTargetMaster() (index int, bootstrapped bool) {
    cs.mu.RLock()
    defer cs.mu.RUnlock()
    return cs.targetMasterIndex, cs.isBootstrapped
}

func (cs *ControllerState) HandleFailover() int {
    cs.mu.Lock()
    defer cs.mu.Unlock()
    // Swap target: 0->1 or 1->0
    cs.targetMasterIndex = 1 - cs.targetMasterIndex
    return cs.targetMasterIndex
}
```

#### 1.2 Add State to Main Controller
```go
// Update MemgraphController struct in controller.go
type MemgraphController struct {
    // ... existing fields
    controllerState *ControllerState
}

func NewMemgraphController(clientset kubernetes.Interface, config *Config) *MemgraphController {
    controller := &MemgraphController{
        // ... existing initialization
        controllerState: &ControllerState{},
    }
    return controller
}
```

**Status**: Complete

---

## **Stage 2: Bootstrap Logic with Master Selection Rules**

**Goal**: Implement safe bootstrap that applies deterministic master selection rules
**Success Criteria**: Controller can determine target master from any cluster state or fail safely
**Tests**: Bootstrap rule tests for all cluster scenarios

### Implementation Tasks

#### 2.1 Master Selection Rules
```go
// pkg/controller/bootstrap.go
func (c *MemgraphController) determineMasterIndex(pods map[string]*PodInfo) (int, error) {
    var mainPods []string
    var replicaPods []string
    
    // Scan ALL pods in the cluster
    for podName, podInfo := range pods {
        switch podInfo.MemgraphRole {
        case "main":
            mainPods = append(mainPods, podName)
        case "replica":
            replicaPods = append(replicaPods, podName)
        }
    }
    
    // Rule 1: If ALL pods are masters (fresh cluster scenario)
    if len(replicaPods) == 0 && len(mainPods) == len(pods) {
        log.Printf("Fresh cluster detected: all %d pods are masters", len(pods))
        log.Printf("Assigning: %s=master, %s=sync, others=async", 
            c.config.GetPodName(0), c.config.GetPodName(1))
        return 0, nil  // Always choose index 0 as master in fresh cluster
    }
    
    // Rule 2: If ANY pod is in replica state (existing cluster)
    if len(replicaPods) > 0 {
        return c.analyzeExistingCluster(mainPods, replicaPods)
    }
    
    // Default to pod-0
    return 0, nil
}

func (c *MemgraphController) analyzeExistingCluster(mainPods, replicaPods []string) (int, error) {
    // Exactly one master - healthy existing cluster
    if len(mainPods) == 1 {
        masterName := mainPods[0]
        masterIndex := c.config.ExtractPodIndex(masterName)
        
        if masterIndex == 0 || masterIndex == 1 {
            return masterIndex, nil
        }
        
        // Master is not an eligible pod (index 0 or 1) - problematic
        return -1, fmt.Errorf("master is %s (index %d, not 0 or 1) - manual intervention required", 
            masterName, masterIndex)
    }
    
    // No masters - all replicas (ambiguous)
    if len(mainPods) == 0 {
        return -1, fmt.Errorf("no master found - all pods are replicas, unclear which has latest data")
    }
    
    // Multiple masters - split brain (ambiguous)
    if len(mainPods) > 1 {
        return -1, fmt.Errorf("split-brain detected: multiple masters %v - manual intervention required", mainPods)
    }
    
    return -1, fmt.Errorf("unexpected cluster state")
}
```

#### 2.2 Bootstrap Process
```go
func (c *MemgraphController) Bootstrap(ctx context.Context) error {
    log.Println("Starting bootstrap process...")
    
    // Discover current cluster state by querying ALL pods
    clusterState, err := c.DiscoverCluster(ctx)
    if err != nil {
        return fmt.Errorf("failed to discover cluster state: %w", err)
    }
    
    // Apply master selection rules
    masterIndex, err := c.determineMasterIndex(clusterState.Pods)
    if err != nil {
        log.Printf("BOOTSTRAP FAILED: %v", err)
        log.Printf("Manual intervention required - cannot safely determine master")
        return fmt.Errorf("ambiguous cluster state during bootstrap: %w", err)
    }
    
    // Remember the decision
    c.controllerState.SetTargetMaster(masterIndex)
    
    log.Printf("Bootstrap complete: target master is %s", c.config.GetPodName(masterIndex))
    return nil
}
```

**Status**: Complete

---

## **Stage 3: SYNC Replica Strategy Implementation**

**Goal**: Implement comprehensive SYNC/ASYNC replica management with health monitoring and emergency procedures
**Success Criteria**: SYNC replica strategy with guaranteed data consistency during failover
**Tests**: SYNC replica health monitoring and emergency promotion tests

### Implementation Tasks

#### 3.1 Enhanced SYNC/ASYNC Replica Registration
```go
// Enhanced RegisterReplicaWithModeAndRetry method supports both SYNC and ASYNC modes
func (mc *MemgraphClient) RegisterReplicaWithModeAndRetry(ctx context.Context, masterBoltAddress, replicaName, replicaAddress, syncMode string) error {
    // Validates syncMode is "SYNC" or "ASYNC"
    // Registers replica with specified mode using retry logic
}
```

#### 3.2 Deterministic SYNC Replica Selection  
```go
// Deterministic selection: First pod alphabetically (memgraph-0 over memgraph-1)
func selectSyncReplica(replicas []*PodInfo) *PodInfo {
    // Sort by pod name alphabetically, return first
    // Ensures consistent SYNC replica choice across controller restarts
}
```

#### 3.3 SYNC Replica Health Monitoring
```go
// 5-level health monitoring system for SYNC replicas
func (c *MemgraphController) monitorSyncReplicaHealth(ctx context.Context, clusterState *ClusterState) {
    // Level 1: Pod running and reachable
    // Level 2: Memgraph process responding  
    // Level 3: Replication role confirmed as "replica"
    // Level 4: Registered in master's SHOW REPLICAS with sync_mode="sync"
    // Level 5: Replication lag acceptable (behind=0 or low)
}
```

#### 3.4 Emergency ASYNC→SYNC Promotion
```go
func (c *MemgraphController) promoteAsyncToSync(ctx context.Context, clusterState *ClusterState, targetReplicaPod string) error {
    // Conservative automation with validation:
    // 1. Drop current replica registration
    // 2. Verify replica is caught up (behind=0)
    // 3. Re-register as SYNC replica
    // 4. Test write operation to confirm SYNC working
}
```

#### 3.5 Controller State Authority Integration
```go
func (c *MemgraphController) configureReplicationWithEnhancedSyncStrategy(ctx context.Context, clusterState *ClusterState) error {
    // Integrates SYNC strategy with controller state tracking
    // Uses target master index for deterministic SYNC replica assignment
    // Handles edge cases like missing eligible pods
}
```

**Status**: Complete

---

## **Stage 4: Enhanced Reconciliation**

**Goal**: Implement smart reconciliation patterns with event filtering and rate limiting
**Success Criteria**: Efficient reconciliation that avoids unnecessary work while ensuring reliability
**Tests**: Reconciliation trigger tests, rate limiting tests, event filtering tests

### Implementation Tasks

#### 4.1 Smart Event Filtering
```go
// Update existing event handlers in controller.go
func (c *MemgraphController) onPodUpdate(oldObj, newObj interface{}) {
    oldPod := oldObj.(*v1.Pod)
    newPod := newObj.(*v1.Pod)
    
    if !c.shouldReconcile(oldPod, newPod) {
        return // Skip unnecessary reconciliation
    }
    
    c.enqueuePodEvent("pod-state-changed")
}

func (c *MemgraphController) shouldReconcile(oldPod, newPod *v1.Pod) bool {
    // Only reconcile for meaningful changes
    meaningfulChanges := []bool{
        oldPod.Status.Phase != newPod.Status.Phase,           // Lifecycle change
        oldPod.Status.PodIP != newPod.Status.PodIP,           // Network change  
        oldPod.DeletionTimestamp != newPod.DeletionTimestamp, // Deletion started
        oldPod.Spec.NodeName != newPod.Spec.NodeName,         // Node migration
    }
    
    for _, hasChange := range meaningfulChanges {
        if hasChange {
            return true
        }
    }
    
    return false
}
```

#### 4.2 Enhanced Error Handling with Rate Limiting
```go
// Update existing reconcileWithBackoff method
func (c *MemgraphController) reconcileWithBackoff(ctx context.Context) error {
    const maxRetries = 3
    const baseDelay = time.Second * 2
    
    for attempt := 0; attempt < maxRetries; attempt++ {
        if attempt > 0 {
            // Exponential backoff with jitter
            delay := time.Duration(float64(baseDelay) * math.Pow(2, float64(attempt-1)))
            jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
            time.Sleep(delay + jitter)
            
            log.Printf("Reconciliation retry %d/%d after %s delay", attempt+1, maxRetries, delay)
        }
        
        err := c.Reconcile(ctx)
        if err == nil {
            if attempt > 0 {
                log.Printf("Reconciliation succeeded on attempt %d", attempt+1)
            }
            return nil
        }
        
        log.Printf("Reconciliation attempt %d failed: %v", attempt+1, err)
        
        // Don't retry on certain types of errors
        if isNonRetryableError(err) {
            return err
        }
    }
    
    return fmt.Errorf("reconciliation failed after %d attempts", maxRetries)
}

func isNonRetryableError(err error) bool {
    // Don't retry bootstrap safety failures - require manual intervention
    return strings.Contains(err.Error(), "manual intervention required") ||
           strings.Contains(err.Error(), "ambiguous cluster state")
}
```

#### 4.3 Reconciliation Metrics and Monitoring
```go
// Add reconciliation metrics tracking
type ReconciliationMetrics struct {
    TotalReconciliations int64
    SuccessfulReconciliations int64
    FailedReconciliations int64
    AverageReconciliationTime time.Duration
    LastReconciliationTime time.Time
    LastReconciliationReason string
}

func (c *MemgraphController) updateReconciliationMetrics(reason string, duration time.Duration, err error) {
    c.mu.Lock()
    defer c.mu.Unlock()
    
    c.metrics.TotalReconciliations++
    c.metrics.LastReconciliationTime = time.Now()
    c.metrics.LastReconciliationReason = reason
    
    if err == nil {
        c.metrics.SuccessfulReconciliations++
    } else {
        c.metrics.FailedReconciliations++
    }
    
    // Update running average
    c.metrics.AverageReconciliationTime = 
        (c.metrics.AverageReconciliationTime + duration) / 2
}
```

**Status**: Complete

---

## **Stage 5: Testing and Validation**

**Goal**: Comprehensive testing of bootstrap rules, operational enforcement, and reconciliation patterns
**Success Criteria**: All scenarios properly tested, edge cases covered, reconciliation efficiency verified
**Tests**: Bootstrap safety tests, failover tests, split-brain resolution tests, reconciliation pattern tests

### Implementation Tasks

#### 5.1 Bootstrap Rule Tests
```go
// pkg/controller/bootstrap_test.go
func TestDetermineMasterIndex(t *testing.T) {
    tests := []struct {
        name           string
        pods           map[string]*PodInfo
        expectedIndex  int
        shouldFail     bool
        expectedError  string
    }{
        {
            name: "fresh_cluster_all_masters",
            pods: map[string]*PodInfo{
                "my-release-memgraph-0": {MemgraphRole: "main"},
                "my-release-memgraph-1": {MemgraphRole: "main"},
                "my-release-memgraph-2": {MemgraphRole: "main"},
            },
            expectedIndex: 0,
            shouldFail:    false,
        },
        {
            name: "healthy_cluster_pod0_master",
            pods: map[string]*PodInfo{
                "my-release-memgraph-0": {MemgraphRole: "main"},
                "my-release-memgraph-1": {MemgraphRole: "replica"},
                "my-release-memgraph-2": {MemgraphRole: "replica"},
            },
            expectedIndex: 0,
            shouldFail:    false,
        },
        {
            name: "failover_cluster_pod1_master",
            pods: map[string]*PodInfo{
                "my-release-memgraph-0": {MemgraphRole: "replica"},
                "my-release-memgraph-1": {MemgraphRole: "main"},
                "my-release-memgraph-2": {MemgraphRole: "replica"},
            },
            expectedIndex: 1,
            shouldFail:    false,
        },
        {
            name: "split_brain_multiple_masters",
            pods: map[string]*PodInfo{
                "my-release-memgraph-0": {MemgraphRole: "main"},
                "my-release-memgraph-1": {MemgraphRole: "replica"},
                "my-release-memgraph-2": {MemgraphRole: "main"},
            },
            expectedIndex: -1,
            shouldFail:    true,
            expectedError: "split-brain detected",
        },
        {
            name: "no_master_all_replicas",
            pods: map[string]*PodInfo{
                "my-release-memgraph-0": {MemgraphRole: "replica"},
                "my-release-memgraph-1": {MemgraphRole: "replica"},
                "my-release-memgraph-2": {MemgraphRole: "replica"},
            },
            expectedIndex: -1,
            shouldFail:    true,
            expectedError: "no master found",
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            controller := &MemgraphController{
                config: &Config{StatefulSetName: "my-release-memgraph"},
            }
            index, err := controller.determineMasterIndex(tt.pods)
            
            if tt.shouldFail {
                assert.Error(t, err)
                assert.Contains(t, err.Error(), tt.expectedError)
            } else {
                assert.NoError(t, err)
                assert.Equal(t, tt.expectedIndex, index)
            }
        })
    }
}
```

#### 5.2 Operational Enforcement Tests
```go
func TestFailoverHandling(t *testing.T) {
    tests := []struct {
        name          string
        initialIndex  int
        masterFailed  bool
        expectedIndex int
    }{
        {
            name:          "pod0_master_fails_promote_pod1",
            initialIndex:  0,
            masterFailed:  true,
            expectedIndex: 1,
        },
        {
            name:          "pod1_master_fails_promote_pod0",
            initialIndex:  1,
            masterFailed:  true,
            expectedIndex: 0,
        },
        {
            name:          "master_healthy_no_change",
            initialIndex:  0,
            masterFailed:  false,
            expectedIndex: 0,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            controller := &MemgraphController{
                controllerState: &ControllerState{
                    targetMasterIndex: tt.initialIndex,
                    isBootstrapped:    true,
                },
            }
            
            if tt.masterFailed {
                newIndex := controller.controllerState.HandleFailover()
                assert.Equal(t, tt.expectedIndex, newIndex)
            }
            
            currentIndex, _ := controller.controllerState.GetTargetMaster()
            assert.Equal(t, tt.expectedIndex, currentIndex)
        })
    }
}
```

#### 5.3 Reconciliation Pattern Tests
```go
func TestSmartEventFiltering(t *testing.T) {
    controller := &MemgraphController{
        config: &Config{StatefulSetName: "test-memgraph"},
    }
    
    tests := []struct {
        name           string
        oldPod         *v1.Pod
        newPod         *v1.Pod
        shouldReconcile bool
    }{
        {
            name: "phase_change_should_reconcile",
            oldPod: &v1.Pod{Status: v1.PodStatus{Phase: v1.PodPending}},
            newPod: &v1.Pod{Status: v1.PodStatus{Phase: v1.PodRunning}},
            shouldReconcile: true,
        },
        {
            name: "ip_change_should_reconcile",
            oldPod: &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.1"}},
            newPod: &v1.Pod{Status: v1.PodStatus{PodIP: "10.0.0.2"}},
            shouldReconcile: true,
        },
        {
            name: "resource_version_only_should_not_reconcile",
            oldPod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
            newPod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"}},
            shouldReconcile: false,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            result := controller.shouldReconcile(tt.oldPod, tt.newPod)
            assert.Equal(t, tt.shouldReconcile, result)
        })
    }
}

func TestReconciliationMetrics(t *testing.T) {
    controller := &MemgraphController{
        metrics: &ReconciliationMetrics{},
    }
    
    // Test successful reconciliation
    controller.updateReconciliationMetrics("test-success", time.Millisecond*100, nil)
    assert.Equal(t, int64(1), controller.metrics.TotalReconciliations)
    assert.Equal(t, int64(1), controller.metrics.SuccessfulReconciliations)
    assert.Equal(t, int64(0), controller.metrics.FailedReconciliations)
    
    // Test failed reconciliation
    controller.updateReconciliationMetrics("test-failure", time.Millisecond*50, fmt.Errorf("test error"))
    assert.Equal(t, int64(2), controller.metrics.TotalReconciliations)
    assert.Equal(t, int64(1), controller.metrics.SuccessfulReconciliations)
    assert.Equal(t, int64(1), controller.metrics.FailedReconciliations)
}
```

#### 5.4 Integration Tests
```go
func TestBootstrapToOperationalFlow(t *testing.T) {
    // Test complete flow from bootstrap through operational enforcement
    controller := &MemgraphController{
        config: &Config{StatefulSetName: "test-memgraph"},
        controllerState: &ControllerState{},
    }
    
    // Simulate fresh cluster
    pods := map[string]*PodInfo{
        "test-memgraph-0": {MemgraphRole: "main"},
        "test-memgraph-1": {MemgraphRole: "main"},
    }
    
    // Bootstrap should succeed and choose pod-0
    index, err := controller.determineMasterIndex(pods)
    assert.NoError(t, err)
    assert.Equal(t, 0, index)
    
    controller.controllerState.SetTargetMaster(index)
    
    // Verify operational state
    targetIndex, isBootstrapped := controller.controllerState.GetTargetMaster()
    assert.True(t, isBootstrapped)
    assert.Equal(t, 0, targetIndex)
}
```

**Status**: Not Started

---

## **Critical Safety Requirements**

### 🔒 **Bootstrap Safety**
- **NEVER** auto-promote in MIXED states during startup
- **ALWAYS** require manual intervention for ambiguous scenarios
- **VALIDATE** cluster state before making any changes

### 🔒 **Master Selection Safety**  
- **NEVER** auto-promote ASYNC replicas over SYNC replicas
- **ALWAYS** prefer existing MAIN node to avoid unnecessary failover
- **REFUSE** unsafe automatic promotion (require manual intervention)

### 🔒 **Split-Brain Prevention**
- **ALWAYS** apply lower-index precedence rule (pod-0 > pod-1 > pod-2)
- **NEVER** allow multiple masters in operational mode
- **ENFORCE** expected topology with controller authority

### 🔒 **Data Loss Prevention**
- **NEVER** promote replicas without data consistency guarantees
- **ALWAYS** prioritize SYNC replicas for master promotion
- **WARN** operators when manual intervention is required

## Implementation Schedule

| Stage | Duration | Dependencies | Risk Level |
|-------|----------|--------------|------------|
| Stage 1: Controller State | 1 day | None | Low |
| Stage 2: Bootstrap Logic | 2 days | Stage 1 | Low |
| Stage 3: Operational Enforcement | 2 days | Stages 1-2 | Medium |
| Stage 4: Enhanced Reconciliation | 1 day | Stages 1-3 | Low |
| Stage 5: Testing & Validation | 1 day | Stages 1-4 | Low |

**Total Duration**: 7 days  
**Critical Path**: Stages 1→2→3→4 (core logic + reconciliation)

## Success Metrics

- ✅ **Zero data loss** during failover scenarios
- ✅ **Deterministic behavior** across controller restarts  
- ✅ **Safe bootstrap** blocking dangerous state transitions
- ✅ **Simple failover** using target master index tracking
- ✅ **Comprehensive testing** covering all failure modes

## Key Benefits of Simplified Approach

### **Compared to Complex 6-Stage Plan:**
- **6 days vs 21 days** - Much faster implementation
- **4 stages vs 6 stages** - Simpler to track and execute
- **Minimal new code** - Leverages existing controller architecture
- **Lower risk** - Smaller changes, easier to test and debug

### **Core Improvements:**
- **Bootstrap Safety** - Refuse ambiguous states during startup
- **State Tracking** - Remember target master decision in memory
- **Deterministic Failover** - Simple index swap (0↔1) for master promotion
- **Existing SYNC Strategy** - Keep current SYNC replica implementation

## Risk Mitigation

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|------------|
| Bootstrap logic bugs | Low | Medium | Comprehensive test coverage for all scenarios |
| Failover timing issues | Low | Medium | Simple index-based logic, hard to get wrong |
| State tracking bugs | Low | Low | Minimal state (just one integer), easy to debug |

This simplified implementation plan fixes the critical controller design flaws with minimal complexity while maintaining all existing functionality and safety guarantees.