# Memgraph HA Helm Chart

A production-ready Helm chart for deploying a high-availability Memgraph cluster with automatic failover capabilities.

## Overview

This chart deploys:
- **3-node Memgraph cluster** with persistent storage
- **Multi-replica controller** with leader election for resilience
- **Integrated gateway** for transparent client connections during failovers
- **Automatic failover** with < 1 second response time
- **Zero data loss** guarantee via SYNC replica strategy

## Architecture

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   memgraph-0    │    │   memgraph-1    │    │   memgraph-2    │
│     (MAIN)      │◄──►│ (SYNC REPLICA)  │◄──►│ (ASYNC REPLICA) │
└─────────────────┘    └─────────────────┘    └─────────────────┘
         ▲                       ▲                       ▲
         └───────────────────────┼───────────────────────┘
                                 ▼
         ┌─────────────────────────────────────────────────┐
         │          Controller/Gateway (3 replicas)        │
         │    • Leader election & state management        │
         │    • Health monitoring & failover detection    │
         │    • Client connection routing & load balancing │
         └─────────────────────────────────────────────────┘
                                 ▲
                                 │ bolt://memgraph-controller:7687
                                 ▼
                    ┌─────────────────────────┐
                    │     Client Applications │
                    └─────────────────────────┘
```

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+
- Persistent Volume provisioner support in the underlying infrastructure
- At least 6Gi available storage (3 pods × 2Gi each)

## Installation

### Add Memgraph Helm Repository

```bash
helm repo add memgraph https://memgraph.github.io/helm-charts
helm repo update
```

### Install the Chart

```bash
# Create namespace
kubectl create namespace memgraph

# Install with default values
helm install memgraph-ha ./charts/memgraph-ha -n memgraph

# Install with custom values
helm install memgraph-ha ./charts/memgraph-ha -n memgraph -f custom-values.yaml
```

## Configuration

### Key Configuration Options

| Parameter | Description | Default |
|-----------|-------------|---------|
| `memgraph.replicaCount` | Number of Memgraph nodes | `3` |
| `memgraph.persistentVolumeClaim.storageSize` | Storage size per node | `10Gi` |
| `memgraph.resources.limits.memory` | Memory limit per Memgraph pod | `2Gi` |
| `controller.replicaCount` | Number of controller replicas | `3` |
| `controller.env.gateway.enabled` | Enable integrated gateway | `true` |
| `controller.env.gateway.maxConnections` | Max concurrent connections | `1000` |
| `controller.env.reconcileInterval` | Health check interval | `30s` |

### Example Custom Values

```yaml
# custom-values.yaml
memgraph:
  replicaCount: 3
  persistentVolumeClaim:
    storageSize: 50Gi
    storageClassName: "fast-ssd"
  resources:
    limits:
      cpu: 2000m
      memory: 4Gi
    requests:
      cpu: 1000m
      memory: 2Gi

controller:
  replicaCount: 3
  env:
    gateway:
      maxConnections: 2000
      rateLimit:
        rps: 200
        burst: 400
  resources:
    limits:
      memory: 1Gi
    requests:
      memory: 256Mi
```

## Usage

### Connecting to the Cluster

Connect to the HA cluster through the controller gateway:

```bash
# Port forward for local access
kubectl port-forward -n memgraph service/memgraph-controller 7687:7687

# Connect using any Bolt-compatible client
bolt://localhost:7687
```

### Using mgconsole

```bash
# Connect to the gateway
kubectl run -it --rm mgconsole --image=memgraph/mgconsole -- mgconsole --host memgraph-controller.memgraph.svc.cluster.local --port 7687

# Or connect directly to a specific pod for debugging
kubectl exec -it memgraph-ha-0 -n memgraph -- mgconsole
```

### Monitoring Cluster Status

```bash
# Check controller status via API
kubectl port-forward -n memgraph service/memgraph-controller 8080:8080
curl http://localhost:8080/api/v1/status | jq

# Check pod status
kubectl get pods -n memgraph -l app.kubernetes.io/name=memgraph

# View controller logs
kubectl logs -n memgraph deployment/memgraph-controller -f
```

## High Availability Features

### Failover Strategy

- **SYNC Replica Strategy**: One SYNC replica guarantees zero data loss
- **Immediate Detection**: Health checks every 30 seconds with immediate failover
- **Automatic Recovery**: Failed nodes automatically rejoin as replicas when healthy

### Topology Management

- **pod-0, pod-1**: Eligible for MAIN or SYNC replica roles
- **pod-2+**: Always ASYNC replicas (read scalability)
- **Controller Authority**: Maintains expected topology across restarts

### Client Connection Handling

- **Transparent Routing**: Gateway automatically routes to current MAIN
- **Connection Pooling**: Up to 1000 concurrent connections
- **Rate Limiting**: 100 RPS with 200 burst capacity
- **Graceful Failover**: Existing connections gracefully migrated

## Operations

### Scaling the Cluster

```bash
# Scale Memgraph nodes (maintains HA topology)
helm upgrade memgraph-ha ./charts/memgraph-ha -n memgraph --set memgraph.replicaCount=5

# Scale controller replicas
helm upgrade memgraph-ha ./charts/memgraph-ha -n memgraph --set controller.replicaCount=5
```

### Backup and Restore

```bash
# Create backup from current MAIN
kubectl exec -n memgraph memgraph-ha-0 -- bash -c 'echo "CREATE SNAPSHOT;" | mgconsole'

# Copy backup files (example for local storage)
kubectl cp memgraph/memgraph-ha-0:/var/lib/memgraph/mg_data/snapshots ./backups/
```

### Troubleshooting

#### Check Cluster Health

```bash
# View controller status
curl http://localhost:8080/api/v1/status | jq '.cluster_state'

# Check replication status directly
kubectl exec memgraph-ha-0 -n memgraph -- bash -c 'echo "SHOW REPLICATION ROLE;" | mgconsole'
kubectl exec memgraph-ha-0 -n memgraph -- bash -c 'echo "SHOW REPLICAS;" | mgconsole'
```

#### Common Issues

**Split-Brain Detection**
```bash
# Controller automatically resolves, but can be monitored via logs
kubectl logs -n memgraph deployment/memgraph-controller | grep "split-brain"
```

**SYNC Replica Failures**
```bash
# Check if SYNC replica is healthy
kubectl get pods -n memgraph -l app.kubernetes.io/name=memgraph
# Controller will automatically promote a healthy ASYNC replica to SYNC
```

**Connection Issues**
```bash
# Test gateway connectivity
kubectl run test-connection --rm -it --image=memgraph/mgconsole -- mgconsole --host memgraph-controller.memgraph.svc.cluster.local
```

## Uninstallation

```bash
# Remove the release
helm uninstall memgraph-ha -n memgraph

# Clean up persistent volumes (if desired)
kubectl delete pvc -n memgraph -l app.kubernetes.io/name=memgraph

# Remove namespace
kubectl delete namespace memgraph
```

## Security

### RBAC Permissions

The controller requires minimal permissions:
- Pod management for health monitoring
- ConfigMap access for state persistence
- Leader election via coordination.k8s.io/leases

### Security Hardening

- Non-root containers with read-only filesystems
- Security contexts with dropped capabilities
- No privilege escalation
- Resource limits and requests enforced

### Network Security

```yaml
# Example NetworkPolicy (not included in chart)
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: memgraph-ha-network-policy
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: memgraph
  policyTypes:
  - Ingress
  ingress:
  - from:
    - podSelector:
        matchLabels:
          app: memgraph-controller
    ports:
    - protocol: TCP
      port: 7687
    - protocol: TCP
      port: 10000
```

## Performance Tuning

### Resource Recommendations

**Production Environment:**
```yaml
memgraph:
  resources:
    limits:
      cpu: "4"
      memory: 8Gi
    requests:
      cpu: "2" 
      memory: 4Gi
  persistentVolumeClaim:
    storageSize: 100Gi
    storageClassName: "fast-ssd"

controller:
  resources:
    limits:
      cpu: "1"
      memory: 1Gi
    requests:
      cpu: 500m
      memory: 512Mi
```

### Storage Performance

- Use SSD-backed storage classes for better performance
- Consider separate storage classes for data vs logs
- Size storage as 4x your dataset size for snapshots

## Monitoring Integration

### Prometheus Metrics

The controller exposes metrics on port 8080:
- Cluster health status
- Failover events
- Gateway connection metrics
- Replication lag (if available)

### Example ServiceMonitor

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: memgraph-controller
spec:
  selector:
    matchLabels:
      app: memgraph-controller
  endpoints:
  - port: http
    path: /metrics
    interval: 30s
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Test with `helm template` and `helm lint`
5. Submit a pull request

## License

This chart is licensed under the Apache License 2.0.

## Support

- **Issues**: Report issues on the project repository
- **Documentation**: Full documentation available at project repository
- **Community**: Join the Memgraph community for support and discussions