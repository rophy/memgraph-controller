# Memgraph Controller Robust Failover Implementation Plan

## Overview

This plan implements the robust master selection and failover strategy documented in CLAUDE.md to fix critical flaws in the current controller design. The current implementation has dangerous failover behavior that can cause data loss and inconsistent cluster states.

## Critical Design Flaws in Current Implementation

### ❌ **Unsafe Bootstrap Behavior**
- **Current**: Processes MIXED states (some main, some replica) without safety checks
- **Risk**: May promote wrong pod, causing data divergence or loss
- **Required**: Refuse to start on ambiguous states during bootstrap

### ❌ **No Controller State Authority**
- **Current**: No persistent tracking of expected topology
- **Risk**: Split-brain scenarios not properly resolved
- **Required**: Maintain expected state and enforce it authoritatively

### ❌ **Inadequate State Classification**  
- **Current**: Simple pod-level state checking
- **Risk**: Cannot detect cluster-wide inconsistencies
- **Required**: Comprehensive cluster state classification

### ❌ **Flawed Master Selection Priority**
- **Current**: Timestamp-based fallback can override SYNC replicas
- **Risk**: ASYNC replicas promoted over SYNC replicas, causing data loss
- **Required**: Strict priority: MAIN > SYNC > REFUSE (no ASYNC auto-promotion)

## Implementation Stages

---

## **Stage 1: Cluster State Classification Engine**

**Goal**: Implement comprehensive cluster state classification for safe decision making
**Success Criteria**: Controller can classify any cluster into well-defined states  
**Tests**: State classification unit tests for all scenarios

### Implementation Tasks

#### 1.1 Define Cluster State Types
```go
// pkg/controller/state_classification.go
type ClusterStateType int

const (
    INITIAL_STATE     ClusterStateType = iota  // All pods are "main" - fresh cluster
    OPERATIONAL_STATE                          // Exactly one master - healthy state  
    MIXED_STATE                               // Some main, some replica - conflicts
    NO_MASTER_STATE                           // No main pods - requires promotion
    SPLIT_BRAIN_STATE                         // Multiple masters - dangerous
)

type ClusterStateClassification struct {
    StateType       ClusterStateType
    MainPods        []string
    ReplicaPods     []string
    Eligibility     PodEligibility
    SafeForBootstrap bool
    RequiresIntervention bool
    Description     string
}

type PodEligibility struct {
    Pod0 bool // memgraph-0 eligible for master/SYNC
    Pod1 bool // memgraph-1 eligible for master/SYNC  
    Pod2Plus []string // pods 2+ always ASYNC only
}
```

#### 1.2 Implement State Classification Logic
```go
func (c *MemgraphController) ClassifyClusterState(pods map[string]*PodInfo) *ClusterStateClassification {
    var mainPods, replicaPods []string
    eligibility := c.determinePodEligibility(pods)
    
    for podName, podInfo := range pods {
        switch podInfo.MemgraphRole {
        case "main":
            mainPods = append(mainPods, podName)
        case "replica":
            replicaPods = append(replicaPods, podName)
        }
    }
    
    // Classify based on master count and distribution
    stateType := c.determineStateType(mainPods, replicaPods, eligibility)
    
    return &ClusterStateClassification{
        StateType:            stateType,
        MainPods:            mainPods,
        ReplicaPods:         replicaPods,
        Eligibility:         eligibility,
        SafeForBootstrap:    c.isSafeForBootstrap(stateType),
        RequiresIntervention: c.requiresIntervention(stateType),
        Description:         c.buildStateDescription(stateType, mainPods, replicaPods),
    }
}
```

#### 1.3 Two-Pod Strategy Implementation
```go
func (c *MemgraphController) determinePodEligibility(pods map[string]*PodInfo) PodEligibility {
    eligibility := PodEligibility{}
    
    for podName := range pods {
        if podName == "memgraph-0" {
            eligibility.Pod0 = true
        } else if podName == "memgraph-1" {
            eligibility.Pod1 = true
        } else {
            eligibility.Pod2Plus = append(eligibility.Pod2Plus, podName)
        }
    }
    
    return eligibility
}
```

**Status**: Not Started

---

## **Stage 2: Bootstrap Safety Controller**

**Goal**: Implement safe bootstrap that refuses dangerous state transitions
**Success Criteria**: Controller blocks startup on unsafe mixed states
**Tests**: Bootstrap safety tests for all dangerous scenarios

### Implementation Tasks

#### 2.1 Bootstrap State Validation
```go
// pkg/controller/bootstrap.go
type BootstrapController struct {
    clientset kubernetes.Interface
    config    *Config
}

func (bc *BootstrapController) ValidateBootstrapSafety(pods map[string]*PodInfo) error {
    classification := bc.classifyState(pods)
    
    switch classification.StateType {
    case INITIAL_STATE, OPERATIONAL_STATE:
        return nil // Safe to proceed
        
    case MIXED_STATE:
        return bc.handleMixedStateDuringBootstrap(classification)
        
    case NO_MASTER_STATE:
        return bc.handleNoMasterDuringBootstrap(classification)
        
    case SPLIT_BRAIN_STATE:
        return bc.handleSplitBrainDuringBootstrap(classification)
    }
    
    return fmt.Errorf("unknown cluster state during bootstrap")
}

func (bc *BootstrapController) handleMixedStateDuringBootstrap(classification *ClusterStateClassification) error {
    log.Error("BOOTSTRAP BLOCKED: Mixed replication state detected")
    log.Error("Found masters: %v", classification.MainPods)
    log.Error("Found replicas: %v", classification.ReplicaPods)
    log.Error("Manual intervention required - cannot determine safe master")
    log.Error("Possible data divergence between pods")
    
    return fmt.Errorf("unsafe mixed state during bootstrap: masters=%v, replicas=%v", 
        classification.MainPods, classification.ReplicaPods)
}
```

#### 2.2 Safe Bootstrap Procedures
```go
func (bc *BootstrapController) InitializeFreshCluster(pods map[string]*PodInfo) (*ExpectedTopology, error) {
    // All pods are "main" - apply deterministic roles
    topology := &ExpectedTopology{
        Master:        "memgraph-0",  // Always choose pod-0 as master
        SyncReplica:   "memgraph-1",  // Always choose pod-1 as SYNC replica
        AsyncReplicas: []string{},    // All other pods become ASYNC
    }
    
    // Add pod-2, pod-3, ... as ASYNC replicas
    for podName := range pods {
        if podName != "memgraph-0" && podName != "memgraph-1" {
            topology.AsyncReplicas = append(topology.AsyncReplicas, podName)
        }
    }
    
    return topology, nil
}

func (bc *BootstrapController) LearnExistingTopology(pods map[string]*PodInfo) (*ExpectedTopology, error) {
    // Learn from existing healthy cluster
    var master string
    for podName, podInfo := range pods {
        if podInfo.MemgraphRole == "main" {
            master = podName
            break
        }
    }
    
    topology := &ExpectedTopology{
        Master: master,
    }
    
    // Determine SYNC vs ASYNC based on current configuration
    // Implementation depends on SHOW REPLICAS parsing
    
    return topology, nil
}
```

#### 2.3 Expected Topology Management
```go
type ExpectedTopology struct {
    Master        string
    SyncReplica   string
    AsyncReplicas []string
    LastUpdated   time.Time
}

func (et *ExpectedTopology) IsValid() bool {
    return et.Master != "" && 
           et.SyncReplica != "" && 
           et.Master != et.SyncReplica
}

func (et *ExpectedTopology) GetExpectedRole(podName string) string {
    if podName == et.Master {
        return "main"
    }
    return "replica"
}

func (et *ExpectedTopology) GetExpectedSyncMode(podName string) string {
    if podName == et.SyncReplica {
        return "sync"
    }
    for _, asyncReplica := range et.AsyncReplicas {
        if podName == asyncReplica {
            return "async"
        }
    }
    return "unknown"
}
```

**Status**: Not Started

---

## **Stage 3: Operational Authority Controller**

**Goal**: Implement authoritative control that enforces expected topology
**Success Criteria**: Controller can resolve split-brain and mixed states during operation
**Tests**: Operational conflict resolution tests

### Implementation Tasks

#### 3.1 Authority State Management
```go
// pkg/controller/authority.go
type AuthorityController struct {
    expectedTopology *ExpectedTopology
    mu               sync.RWMutex
    isBootstrapped   bool
}

func (ac *AuthorityController) SetExpectedTopology(topology *ExpectedTopology) {
    ac.mu.Lock()
    defer ac.mu.Unlock()
    
    ac.expectedTopology = topology
    ac.expectedTopology.LastUpdated = time.Now()
    ac.isBootstrapped = true
    
    log.Printf("Controller authority established: Master=%s, SYNC=%s, ASYNC=%v", 
        topology.Master, topology.SyncReplica, topology.AsyncReplicas)
}

func (ac *AuthorityController) EnforceTopology(ctx context.Context, currentState map[string]*PodInfo) error {
    ac.mu.RLock()
    defer ac.mu.RUnlock()
    
    if !ac.isBootstrapped {
        return fmt.Errorf("authority not established - bootstrap required")
    }
    
    conflicts := ac.detectTopologyConflicts(currentState)
    if len(conflicts) == 0 {
        return nil // No conflicts to resolve
    }
    
    return ac.resolveConflicts(ctx, conflicts, currentState)
}
```

#### 3.2 Conflict Detection and Resolution
```go
type TopologyConflict struct {
    Type        string
    PodName     string
    Expected    string
    Actual      string
    Resolution  string
}

func (ac *AuthorityController) detectTopologyConflicts(currentState map[string]*PodInfo) []TopologyConflict {
    var conflicts []TopologyConflict
    
    for podName, podInfo := range currentState {
        expectedRole := ac.expectedTopology.GetExpectedRole(podName)
        actualRole := podInfo.MemgraphRole
        
        if expectedRole != actualRole {
            conflicts = append(conflicts, TopologyConflict{
                Type:       "role_mismatch",
                PodName:    podName,
                Expected:   expectedRole,
                Actual:     actualRole,
                Resolution: ac.determineResolution(podName, expectedRole, actualRole),
            })
        }
    }
    
    return conflicts
}

func (ac *AuthorityController) resolveConflicts(ctx context.Context, conflicts []TopologyConflict, currentState map[string]*PodInfo) error {
    log.Printf("Resolving %d topology conflicts with authority", len(conflicts))
    
    for _, conflict := range conflicts {
        log.Printf("Resolving conflict: %s expected=%s actual=%s resolution=%s", 
            conflict.PodName, conflict.Expected, conflict.Actual, conflict.Resolution)
            
        if err := ac.applyResolution(ctx, conflict, currentState); err != nil {
            return fmt.Errorf("failed to resolve conflict for %s: %w", conflict.PodName, err)
        }
    }
    
    return nil
}
```

#### 3.3 Lower-Index Precedence Resolution
```go
func (ac *AuthorityController) ResolveSplitBrain(ctx context.Context, masters []string) error {
    if len(masters) <= 1 {
        return nil // No split-brain
    }
    
    log.Printf("SPLIT-BRAIN DETECTED: Multiple masters found: %v", masters)
    
    // Apply lower-index precedence rule
    keepMaster := ac.findLowestIndexMaster(masters)
    demoteMasters := ac.filterMasters(masters, keepMaster)
    
    log.Printf("Keeping master: %s, demoting: %v", keepMaster, demoteMasters)
    
    for _, demoteMaster := range demoteMasters {
        if err := ac.demoteToReplica(ctx, demoteMaster); err != nil {
            return fmt.Errorf("failed to demote master %s: %w", demoteMaster, err)
        }
        
        // Register as appropriate replica type
        if ac.isPodEligibleForSync(demoteMaster) {
            ac.registerAsSyncReplica(ctx, keepMaster, demoteMaster)
        } else {
            ac.registerAsAsyncReplica(ctx, keepMaster, demoteMaster)
        }
    }
    
    // Update expected topology
    ac.updateTopologyAfterSplitBrainResolution(keepMaster)
    
    return nil
}

func (ac *AuthorityController) findLowestIndexMaster(masters []string) string {
    // Extract pod index from name (e.g., "memgraph-0" -> 0)
    lowestIndex := int(^uint(0) >> 1) // Max int
    lowestMaster := ""
    
    for _, master := range masters {
        index := ac.extractPodIndex(master)
        if index < lowestIndex {
            lowestIndex = index
            lowestMaster = master
        }
    }
    
    return lowestMaster
}
```

**Status**: Not Started

---

## **Stage 4: Enhanced Master Selection Logic**

**Goal**: Implement strict priority-based master selection that prevents data loss
**Success Criteria**: SYNC replicas always preferred over ASYNC, no unsafe auto-promotion
**Tests**: Master selection priority tests

### Implementation Tasks

#### 4.1 Priority-Based Master Selection
```go
// pkg/controller/master_selection.go
type MasterSelectionController struct {
    authorityController *AuthorityController
}

func (msc *MasterSelectionController) SelectMaster(ctx context.Context, pods map[string]*PodInfo, isBootstrap bool) (*MasterSelectionResult, error) {
    if isBootstrap {
        return msc.selectMasterForBootstrap(pods)
    }
    return msc.selectMasterForOperation(ctx, pods)
}

type MasterSelectionResult struct {
    SelectedMaster   string
    SelectionReason  string
    RequiresPromotion bool
    SafeAutoPromotion bool
    Warnings         []string
}

func (msc *MasterSelectionController) selectMasterForOperation(ctx context.Context, pods map[string]*PodInfo) (*MasterSelectionResult, error) {
    // Priority 1: Existing MAIN node (avoid unnecessary failover)
    if existingMain := msc.findExistingMain(pods); existingMain != "" {
        return &MasterSelectionResult{
            SelectedMaster:    existingMain,
            SelectionReason:   "existing MAIN node (avoid unnecessary failover)",
            RequiresPromotion: false,
            SafeAutoPromotion: true,
        }, nil
    }
    
    // Priority 2: SYNC replica (guaranteed data consistency)
    if syncReplica := msc.findSyncReplica(pods); syncReplica != "" {
        return &MasterSelectionResult{
            SelectedMaster:    syncReplica,
            SelectionReason:   "SYNC replica promotion (guaranteed consistency)",
            RequiresPromotion: true,
            SafeAutoPromotion: true,
            Warnings:         []string{"Promoting SYNC replica to master"},
        }, nil
    }
    
    // Priority 3: REFUSE - No safe automatic promotion
    return &MasterSelectionResult{
        SelectedMaster:    "",
        SelectionReason:   "no safe automatic promotion possible (SYNC replica unavailable)",
        RequiresPromotion: false,
        SafeAutoPromotion: false,
        Warnings: []string{
            "CRITICAL: No SYNC replica available for safe automatic promotion",
            "CRITICAL: Cannot guarantee data consistency - manual intervention required",
            "CRITICAL: ASYNC replicas may be missing committed transactions",
        },
    }, fmt.Errorf("no safe automatic master promotion possible")
}
```

#### 4.2 Master Failure Scenarios
```go
func (msc *MasterSelectionController) HandleMasterFailure(ctx context.Context, failedMaster string, pods map[string]*PodInfo) error {
    log.Printf("MASTER FAILURE DETECTED: %s is unreachable", failedMaster)
    
    // Remove failed master from consideration
    availablePods := make(map[string]*PodInfo)
    for podName, podInfo := range pods {
        if podName != failedMaster {
            availablePods[podName] = podInfo
        }
    }
    
    // Select new master using strict priority
    result, err := msc.selectMasterForOperation(ctx, availablePods)
    if err != nil {
        log.Printf("Cannot safely promote new master: %v", err)
        return msc.handleUnsafeFailoverScenario(failedMaster, availablePods)
    }
    
    if !result.SafeAutoPromotion {
        return fmt.Errorf("master failure requires manual intervention: %s", result.SelectionReason)
    }
    
    // Execute safe master promotion
    return msc.promoteMaster(ctx, result.SelectedMaster, availablePods)
}

func (msc *MasterSelectionController) handleUnsafeFailoverScenario(failedMaster string, availablePods map[string]*PodInfo) error {
    log.Printf("UNSAFE FAILOVER SCENARIO:")
    log.Printf("  Failed master: %s", failedMaster)
    log.Printf("  Available pods: %v", msc.getPodNames(availablePods))
    log.Printf("  No SYNC replica available for safe promotion")
    log.Printf("")
    log.Printf("MANUAL INTERVENTION REQUIRED:")
    log.Printf("  1. Verify data integrity on available replicas")
    log.Printf("  2. Manually promote pod with latest data")
    log.Printf("  3. Accept potential data loss risk")
    
    return fmt.Errorf("unsafe failover scenario - manual intervention required")
}
```

#### 4.3 SYNC Replica Recovery After Master Failure
```go
func (msc *MasterSelectionController) HandleOldMasterReturn(ctx context.Context, oldMaster string, currentMaster string, pods map[string]*PodInfo) error {
    log.Printf("OLD MASTER RETURNED: %s (current master: %s)", oldMaster, currentMaster)
    
    // Apply lower-index precedence rule for split-brain resolution
    oldMasterIndex := msc.extractPodIndex(oldMaster)
    currentMasterIndex := msc.extractPodIndex(currentMaster)
    
    if oldMasterIndex < currentMasterIndex {
        log.Printf("Old master %s has lower index than current master %s - restoring original master", oldMaster, currentMaster)
        return msc.restoreOriginalMaster(ctx, oldMaster, currentMaster, pods)
    } else {
        log.Printf("Current master %s has lower index than old master %s - keeping current master", currentMaster, oldMaster)
        return msc.demoteOldMaster(ctx, oldMaster, currentMaster, pods)
    }
}

func (msc *MasterSelectionController) restoreOriginalMaster(ctx context.Context, originalMaster, temporaryMaster string, pods map[string]*PodInfo) error {
    log.Printf("Restoring original master: %s", originalMaster)
    
    // Step 1: Demote temporary master to replica
    if err := msc.demoteToReplica(ctx, temporaryMaster); err != nil {
        return fmt.Errorf("failed to demote temporary master %s: %w", temporaryMaster, err)
    }
    
    // Step 2: Promote original master (it should already be MAIN, but ensure)
    if err := msc.promoteToMaster(ctx, originalMaster); err != nil {
        return fmt.Errorf("failed to restore original master %s: %w", originalMaster, err)
    }
    
    // Step 3: Rebuild replication topology
    return msc.rebuildReplicationTopology(ctx, originalMaster, temporaryMaster, pods)
}
```

**Status**: Not Started

---

## **Stage 5: Integration and Safety Testing**

**Goal**: Integrate all components and verify safety guarantees through comprehensive testing
**Success Criteria**: All dangerous scenarios are safely handled, no data loss scenarios  
**Tests**: End-to-end safety tests for all failure scenarios

### Implementation Tasks

#### 5.1 Integration Controller
```go
// pkg/controller/robust_controller.go
type RobustMemgraphController struct {
    // Core components
    clientset            kubernetes.Interface
    config              *Config
    
    // New robust components
    stateClassifier     *StateClassificationController
    bootstrapController *BootstrapController
    authorityController *AuthorityController
    masterSelector      *MasterSelectionController
    
    // Original components (enhanced)
    podDiscovery        *PodDiscovery
    memgraphClient      *MemgraphClient
    httpServer          *HTTPServer
    
    // State tracking
    isBootstrapped      bool
    expectedTopology    *ExpectedTopology
    lastSuccessfulState *ClusterStateClassification
}

func NewRobustMemgraphController(clientset kubernetes.Interface, config *Config) *RobustMemgraphController {
    controller := &RobustMemgraphController{
        clientset:           clientset,
        config:             config,
        podDiscovery:       NewPodDiscovery(clientset, config),
        memgraphClient:     NewMemgraphClient(config),
        stateClassifier:    NewStateClassificationController(),
        bootstrapController: NewBootstrapController(clientset, config),
        authorityController: NewAuthorityController(),
        masterSelector:     NewMasterSelectionController(),
    }
    
    controller.httpServer = NewHTTPServer(controller, config)
    return controller
}
```

#### 5.2 Robust Bootstrap Process
```go
func (rmc *RobustMemgraphController) Bootstrap(ctx context.Context) error {
    log.Println("Starting robust bootstrap process...")
    
    // Step 1: Discover current cluster state
    pods, err := rmc.podDiscovery.DiscoverPods(ctx)
    if err != nil {
        return fmt.Errorf("failed to discover pods during bootstrap: %w", err)
    }
    
    // Step 2: Query Memgraph state for all pods
    if err := rmc.queryMemgraphState(ctx, pods.Pods); err != nil {
        return fmt.Errorf("failed to query Memgraph state during bootstrap: %w", err)
    }
    
    // Step 3: Classify cluster state
    classification := rmc.stateClassifier.ClassifyClusterState(pods.Pods)
    log.Printf("Bootstrap cluster state classification: %s", classification.Description)
    
    // Step 4: Validate bootstrap safety
    if err := rmc.bootstrapController.ValidateBootstrapSafety(pods.Pods); err != nil {
        return fmt.Errorf("bootstrap safety validation failed: %w", err)
    }
    
    // Step 5: Establish expected topology
    expectedTopology, err := rmc.establishExpectedTopology(ctx, classification, pods.Pods)
    if err != nil {
        return fmt.Errorf("failed to establish expected topology: %w", err)
    }
    
    // Step 6: Set controller authority
    rmc.authorityController.SetExpectedTopology(expectedTopology)
    rmc.isBootstrapped = true
    
    log.Printf("Bootstrap completed successfully: Master=%s, SYNC=%s", 
        expectedTopology.Master, expectedTopology.SyncReplica)
    
    return nil
}
```

#### 5.3 Robust Operational Loop
```go
func (rmc *RobustMemgraphController) RobustReconcile(ctx context.Context) error {
    if !rmc.isBootstrapped {
        return rmc.Bootstrap(ctx)
    }
    
    // Step 1: Discover current state
    pods, err := rmc.podDiscovery.DiscoverPods(ctx)
    if err != nil {
        return fmt.Errorf("failed to discover pods: %w", err)
    }
    
    // Step 2: Query Memgraph state
    if err := rmc.queryMemgraphState(ctx, pods.Pods); err != nil {
        return fmt.Errorf("failed to query Memgraph state: %w", err)
    }
    
    // Step 3: Classify current state
    classification := rmc.stateClassifier.ClassifyClusterState(pods.Pods)
    
    // Step 4: Enforce expected topology with authority
    if err := rmc.authorityController.EnforceTopology(ctx, pods.Pods); err != nil {
        return fmt.Errorf("failed to enforce topology: %w", err)
    }
    
    // Step 5: Handle state-specific scenarios
    return rmc.handleOperationalState(ctx, classification, pods.Pods)
}

func (rmc *RobustMemgraphController) handleOperationalState(ctx context.Context, classification *ClusterStateClassification, pods map[string]*PodInfo) error {
    switch classification.StateType {
    case OPERATIONAL_STATE:
        return rmc.handleHealthyState(ctx, pods)
        
    case MIXED_STATE:
        return rmc.handleMixedStateWithAuthority(ctx, classification, pods)
        
    case SPLIT_BRAIN_STATE:
        return rmc.handleSplitBrainWithAuthority(ctx, classification, pods)
        
    case NO_MASTER_STATE:
        return rmc.handleMasterFailure(ctx, classification, pods)
        
    default:
        return fmt.Errorf("unhandled operational state: %s", classification.Description)
    }
}
```

#### 5.4 Comprehensive Safety Tests
```go
// pkg/controller/robust_controller_test.go

// Test bootstrap safety
func TestBootstrapSafety(t *testing.T) {
    tests := []struct {
        name     string
        pods     map[string]*PodInfo
        shouldFail bool
        reason   string
    }{
        {
            name: "safe_fresh_cluster",
            pods: map[string]*PodInfo{
                "memgraph-0": {MemgraphRole: "main"},
                "memgraph-1": {MemgraphRole: "main"},
                "memgraph-2": {MemgraphRole: "main"},
            },
            shouldFail: false,
            reason: "all pods main - safe deterministic assignment",
        },
        {
            name: "unsafe_mixed_state",
            pods: map[string]*PodInfo{
                "memgraph-0": {MemgraphRole: "main"},
                "memgraph-1": {MemgraphRole: "replica"},
                "memgraph-2": {MemgraphRole: "main"},
            },
            shouldFail: true,
            reason: "mixed state - data divergence risk",
        },
        {
            name: "unsafe_no_master",
            pods: map[string]*PodInfo{
                "memgraph-0": {MemgraphRole: "replica"},
                "memgraph-1": {MemgraphRole: "replica"},
                "memgraph-2": {MemgraphRole: "replica"},
            },
            shouldFail: true,
            reason: "no master - unclear data freshness",
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            controller := NewRobustMemgraphController(nil, &Config{})
            err := controller.bootstrapController.ValidateBootstrapSafety(tt.pods)
            
            if tt.shouldFail && err == nil {
                t.Errorf("Expected bootstrap to fail for %s but it succeeded", tt.reason)
            }
            if !tt.shouldFail && err != nil {
                t.Errorf("Expected bootstrap to succeed for %s but it failed: %v", tt.reason, err)
            }
        })
    }
}

// Test master selection priority
func TestMasterSelectionPriority(t *testing.T) {
    tests := []struct {
        name           string
        pods           map[string]*PodInfo
        expectedMaster string
        shouldSucceed  bool
    }{
        {
            name: "prefer_existing_main",
            pods: map[string]*PodInfo{
                "memgraph-0": {MemgraphRole: "main"},
                "memgraph-1": {MemgraphRole: "replica", IsSyncReplica: true},
            },
            expectedMaster: "memgraph-0",
            shouldSucceed:  true,
        },
        {
            name: "promote_sync_replica",
            pods: map[string]*PodInfo{
                "memgraph-0": {MemgraphRole: "replica", IsSyncReplica: true},
                "memgraph-1": {MemgraphRole: "replica", IsSyncReplica: false},
            },
            expectedMaster: "memgraph-0",
            shouldSucceed:  true,
        },
        {
            name: "refuse_async_only",
            pods: map[string]*PodInfo{
                "memgraph-0": {MemgraphRole: "replica", IsSyncReplica: false},
                "memgraph-1": {MemgraphRole: "replica", IsSyncReplica: false},
            },
            expectedMaster: "",
            shouldSucceed:  false,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            controller := NewRobustMemgraphController(nil, &Config{})
            result, err := controller.masterSelector.selectMasterForOperation(context.TODO(), tt.pods)
            
            if tt.shouldSucceed && err != nil {
                t.Errorf("Expected success but got error: %v", err)
            }
            if !tt.shouldSucceed && err == nil {
                t.Errorf("Expected failure but got success")
            }
            if tt.shouldSucceed && result.SelectedMaster != tt.expectedMaster {
                t.Errorf("Expected master %s but got %s", tt.expectedMaster, result.SelectedMaster)
            }
        })
    }
}
```

**Status**: Not Started

---

## **Stage 6: Migration and Deployment**

**Goal**: Deploy robust controller while maintaining backward compatibility
**Success Criteria**: Smooth migration from current implementation to robust version
**Tests**: Migration safety tests, production deployment validation

### Implementation Tasks

#### 6.1 Backward Compatibility Layer
```go
// pkg/controller/migration.go
type MigrationController struct {
    robustController *RobustMemgraphController
    legacyController *MemgraphController
    migrationConfig  *MigrationConfig
}

type MigrationConfig struct {
    EnableRobustMode     bool
    MigrationPhase      string // "preparation", "migration", "completion"
    SafetyValidation    bool
    RollbackOnFailure   bool
}

func (mc *MigrationController) MigrateToRobustController(ctx context.Context) error {
    log.Println("Starting migration to robust controller...")
    
    // Phase 1: Validate current state is stable
    if err := mc.validateCurrentState(ctx); err != nil {
        return fmt.Errorf("current state validation failed: %w", err)
    }
    
    // Phase 2: Initialize robust controller with current state
    if err := mc.initializeRobustController(ctx); err != nil {
        return fmt.Errorf("robust controller initialization failed: %w", err)
    }
    
    // Phase 3: Switch to robust mode
    if err := mc.switchToRobustMode(ctx); err != nil {
        return fmt.Errorf("failed to switch to robust mode: %w", err)
    }
    
    log.Println("Migration to robust controller completed successfully")
    return nil
}
```

#### 6.2 Deployment Safety
```go
func (mc *MigrationController) validateCurrentState(ctx context.Context) error {
    // Check current cluster health
    clusterState, err := mc.legacyController.DiscoverCluster(ctx)
    if err != nil {
        return fmt.Errorf("failed to discover current cluster state: %w", err)
    }
    
    // Ensure cluster is stable (no ongoing replication issues)
    if err := mc.validateClusterStability(clusterState); err != nil {
        return fmt.Errorf("cluster stability check failed: %w", err)
    }
    
    // Verify SYNC replica health if configured
    if err := mc.validateSyncReplicaHealth(ctx, clusterState); err != nil {
        return fmt.Errorf("SYNC replica health check failed: %w", err)
    }
    
    return nil
}
```

#### 6.3 Rollback Procedures
```go
func (mc *MigrationController) RollbackToLegacyController(ctx context.Context) error {
    log.Println("Rolling back to legacy controller...")
    
    // Preserve current replication state
    currentState, err := mc.robustController.GetClusterStatus(ctx)
    if err != nil {
        log.Printf("Warning: Could not get current state for rollback: %v", err)
    }
    
    // Switch back to legacy mode
    mc.migrationConfig.EnableRobustMode = false
    
    // Validate rollback safety
    if err := mc.validateRollbackSafety(ctx, currentState); err != nil {
        return fmt.Errorf("rollback safety validation failed: %w", err)
    }
    
    log.Println("Rollback to legacy controller completed")
    return nil
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
| Stage 1 | 3 days | None | Low |
| Stage 2 | 4 days | Stage 1 | Medium |
| Stage 3 | 5 days | Stages 1-2 | Medium |
| Stage 4 | 4 days | Stages 1-3 | High |
| Stage 5 | 3 days | Stages 1-4 | High |
| Stage 6 | 2 days | Stages 1-5 | Low |

**Total Duration**: 21 days  
**Critical Path**: Stages 1→2→3→4 (foundation components)

## Success Metrics

- ✅ **Zero data loss** during failover scenarios
- ✅ **Deterministic behavior** across controller restarts  
- ✅ **Safe bootstrap** blocking dangerous state transitions
- ✅ **Authoritative control** resolving split-brain scenarios
- ✅ **Comprehensive testing** covering all failure modes

## Risk Mitigation

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|------------|
| Migration failure | Medium | High | Comprehensive rollback procedures |
| Data corruption | Low | Critical | Bootstrap safety validation |
| Split-brain bugs | Medium | High | Extensive split-brain testing |
| SYNC replica issues | High | Medium | Emergency procedures documented |

This implementation plan addresses all critical flaws in the current controller design and establishes robust, safe master selection and failover behavior that prevents data loss and ensures cluster consistency.