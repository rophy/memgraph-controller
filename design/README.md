# Memgraph Controller Design Documentation

This directory contains the technical design specifications for the Memgraph Controller, which manages high-availability Memgraph clusters in Kubernetes.

## Document Structure

### Core Design Documents

1. **[architecture.md](./architecture.md)** - System Architecture & Components
   - High-level architecture overview
   - MAIN-SYNC-ASYNC replication topology
   - Key design principles
   - Terminology and definitions
   - Controller high availability (leader election)
   - Gateway design
   - Deployment characteristics

2. **[reconciliation.md](./reconciliation.md)** - Reconciliation Logic
   - Controller code flow
   - Reconciliation loop
   - Reconcile actions
   - Cluster state discovery
   - Cluster initialization
   - Event-driven reconciliation

3. **[failover.md](./failover.md)** - Failover Strategy & Implementation
   - Failover actions and triggers
   - Failover event queue architecture
   - Connection management during failover
   - Recovery procedures

### Supplementary Documents

- **[context-timeout-strategy.md](./context-timeout-strategy.md)** - Context timeout implementation strategy

## Quick Navigation

### For Understanding the System
Start with [architecture.md](./architecture.md) to understand the overall system design and components.

### For Implementation Details
- Reconciliation logic: See [reconciliation.md](./reconciliation.md)
- Failover behavior: See [failover.md](./failover.md)

### For Operations
- Leader election: See "Controller High Availability" in [architecture.md](./architecture.md)
- Gateway behavior: See "Gateway Design" in [architecture.md](./architecture.md)
- Event handling: See "Actions to Kubernetes Events" in [reconciliation.md](./reconciliation.md)

## Design Principles

The controller follows these core principles:

1. **Two-Pod Authority**: Only pod-0 and pod-1 are eligible for MAIN/SYNC replica roles
2. **SYNC Replica Strategy**: One SYNC replica ensures zero data loss during failover
3. **Design-Contract-Based Failover**: Predictable topology eliminates runtime discovery
4. **Immediate Failover**: Sub-second failover with automatic gateway coordination
5. **Write Conflict Protection**: SYNC replication prevents dual-MAIN scenarios

## Implementation Compliance

All code implementations MUST comply with these design documents. See [../CLAUDE.md](../CLAUDE.md) for the design compliance framework that ensures code aligns with these specifications.