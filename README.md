# Memgraph Controller with Gateway

A Kubernetes controller that manages Memgraph clusters with built-in TCP gateway for transparent failover.

> **📋 Design Reference**: See [design/MEMGRAPH_HA.md](./design/MEMGRAPH_HA.md) for detailed architecture and implementation specifications.

## Features

- **MAIN-SYNC-ASYNC Topology**: Zero data loss failover with write conflict protection
- **Two-Pod Authority**: Only pod-0 and pod-1 eligible for MAIN/SYNC replica roles  
- **Immediate Failover**: Sub-second failover response with automatic gateway coordination
- **Bootstrap Safety**: Conservative startup that refuses ambiguous cluster states
- **Transparent Gateway**: Raw TCP proxy with automatic connection management

## Quick Start

### Prerequisites

- Kubernetes cluster with kubectl access
- Memgraph Helm chart repository
- Docker for building custom images

### Basic Deployment

1. **Deploy with Helm:**
```bash
helm repo add memgraph https://memgraph.github.io/helm-charts
helm install memgraph-ha memgraph/memgraph-ha
```

2. **Verify Deployment:**
```bash
kubectl get pods -n memgraph
kubectl logs -l app=memgraph-controller -n memgraph
```

3. **Connect via Gateways:**
```bash
# Read/Write Gateway - routes to current MAIN
kubectl port-forward svc/memgraph-gateway 7687:7687 -n memgraph

# Read-Only Gateway - routes to replicas (optional)
kubectl port-forward svc/memgraph-gateway-read 7688:7687 -n memgraph
```

### Configuration

Key environment variables for the controller:

```bash
# Gateway settings
GATEWAY_ENABLED=true
ENABLE_READ_GATEWAY=true                  # Enable read-only gateway
GATEWAY_BIND_ADDRESS=0.0.0.0:7687         # RW Gateway
GATEWAY_MAX_CONNECTIONS=1000

# Controller settings
RECONCILE_INTERVAL=30s
LOG_LEVEL=info
```

## Architecture

The controller implements a **MAIN-SYNC-ASYNC** replication topology, with gateway for routing traffic to main (readwrite) and replica (read-only).

```mermaid
graph  LR
  write-client([read-clients]) -->|bolt://gateway:7687| service
  read-client([write-clients])  -->|bolt://gateway-read:7687| service-rw
  subgraph Namespace
    service[Service <br> gateway] -->|:7687| controller
    service-rw[Service <br> gateway-read] -->|:7688| controller
    controller[Deployment <br> controller-gateway] --> main
    controller --> async-replica
    subgraph Statefulset memgraph
        main[Role: Main] -.-> sync-replica
        main -.-> async-replica
        sync-replica[Role: Sync Replica]
        async-replica[Role: Async Replica]
    end
  end

  linkStyle default stroke-width:2px
  linkStyle 0 stroke:red
  linkStyle 1 stroke:red
  linkStyle 2 stroke:red
  linkStyle 3 stroke:green
  linkStyle 4 stroke:red
  linkStyle 5 stroke:green

  classDef k8s fill:#326ce5,stroke:#fff,stroke-width:4px,color:#fff;
  classDef cluster fill:#fff,stroke:#bbb,stroke-width:2px,color:#326ce5;
  class service,service-rw,controller k8s;
  class namespace cluster
```

**Key Benefits:**
- **Zero Data Loss**: SYNC replica ensures all committed transactions are replicated
- **Write Protection**: SYNC replication prevents dual-MAIN scenarios
- **Fast Failover**: SYNC replica can be promoted immediately without data loss
- **Client Transparency**: Gateway handles connection routing automatically

## Gateway Integration

The controller includes **two embedded TCP gateways** that provide transparent failover and read scaling for client connections:

### Dual Gateway Architecture

**Read/Write Gateway (Port 7687)**
- Always routes to the current MAIN pod
- Ensures strong consistency for all writes

**Read-Only Gateway (Port 7688)**
- Always routes to the healthy ASYNC replica
- Fall back to SYNC replica in case ASYNC replica not ready
- Internal controller port: 7688, External service port: 7687 (for consistency)

### Features
- **Transparent Proxying**: Raw TCP proxy with no protocol interpretation
- **Automatic Failover**: Connection termination and re-routing on topology changes
- **Connection Tracking**: Full session lifecycle management with metrics
- **Health Monitoring**: Continuous connectivity validation and error rate tracking

### Configuration
```bash
# Enable dual gateway functionality
GATEWAY_ENABLED=true
ENABLE_READ_GATEWAY=true
GATEWAY_BIND_ADDRESS=0.0.0.0:7687        # RW Gateway
GATEWAY_READ_BIND_ADDRESS=0.0.0.0:7688    # Read Gateway (internal)
GATEWAY_MAX_CONNECTIONS=1000
GATEWAY_TIMEOUT=30s
```

## Monitoring

The controller exposes comprehensive metrics through its HTTP API:

- **Cluster State**: MAIN/replica roles, replication status
- **Gateway Stats**: Active connections, failover count, error rates  
- **Health Status**: MAIN connectivity, error thresholds, system health

Access metrics at: `http://controller:8080/status`

## Memgraph Specifications

For detailed Memgraph Community Edition specifications, commands, and replication reference, see [STUDY_NOTES.md](./STUDY_NOTES.md).
