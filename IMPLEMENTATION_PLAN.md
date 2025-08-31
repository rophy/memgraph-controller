# Implementation Plan - DESIGN.md 100% Compliance Refactor

## Objective
Achieve 100% compliance with DESIGN.md specification while reducing code complexity through focused, incremental refactoring.

## Stage 1: Implement Exact Reconcile Actions Flow
**Goal**: Create reconcile_actions.go implementing DESIGN.md lines 75-100 exactly  
**Success Criteria**: 
- All 8 steps from DESIGN.md "Reconcile Actions" executed in exact order
- Each step references its DESIGN.md line number in comments
- Unit tests validate each step independently
**Tests**: 
- Test each reconcile step with mock data
- Verify step ordering matches DESIGN.md
- Test data_info ready/not-ready conditions
**Status**: Not Started

## Stage 2: Add ASYNC Replica Health Monitoring  
**Goal**: Implement missing ASYNC replica data_info checks per DESIGN.md
**Success Criteria**:
- Step 6: Drop ASYNC replicas when data_info not "ready"
- Step 8.2: Log warnings for unhealthy ASYNC replicas in final check
- Health monitoring runs on every reconciliation cycle
**Tests**:
- Test ASYNC replica drops when data_info unhealthy
- Test warning logs generated for unhealthy ASYNC replicas
- E2E test with simulated ASYNC replica failure
**Status**: Not Started

## Stage 3: Reduce Controller Complexity
**Goal**: Split controller.go (1215 lines) into focused, manageable files
**Success Criteria**:
- controller_core.go: Core struct and initialization (~200 lines)
- controller_reconcile.go: Reconciliation loop (~150 lines)  
- controller_events.go: Event handling (~200 lines)
- controller_discovery.go: Cluster discovery (~150 lines)
- No file exceeds 300 lines
**Tests**:
- All existing tests pass after refactoring
- No change in functionality
**Status**: Not Started

## Stage 4: Simplify Event Processing
**Goal**: Remove complex event logic not specified in DESIGN.md
**Success Criteria**:
- Event handlers only enqueue for reconciliation (no immediate actions)
- Remove immediate failover logic not in DESIGN.md
- Reconciliation loop is sole source of state changes
**Tests**:
- Verify events only trigger reconciliation
- Test that failover only happens during reconciliation
- E2E test confirming no immediate state changes from events
**Status**: Not Started

## Stage 5: Add Design Compliance Validation
**Goal**: Create automated validation ensuring code matches DESIGN.md
**Success Criteria**:
- validateDesignCompliance() function checks all DESIGN.md requirements
- Validation runs at start of each reconciliation
- Clear error messages reference specific DESIGN.md sections
- CI pipeline includes design compliance check
**Tests**:
- Test validation catches DESIGN.md violations
- Test all valid states pass validation
- Integration with existing test suite
**Status**: Not Started

## Implementation Order
1. Stage 1 first - establishes correct reconciliation flow
2. Stage 2 next - adds missing ASYNC monitoring 
3. Stage 3 & 4 in parallel - code organization improvements
4. Stage 5 last - locks in compliance

## Notes
- Each stage must pass all tests before moving to next
- Update this document's Status field after completing each stage
- Remove this file when all stages complete