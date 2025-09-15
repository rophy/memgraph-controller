# Test Client with Command-Driven Architecture

A Python client for Memgraph that supports multiple modes: write, query, logs, and idle.

## Features

- **Command-Driven**: Supports write, query, logs, and idle modes
- **Continuous Write Loop**: Writes data to Memgraph at configurable intervals
- **Comprehensive Metrics**: Tracks success/error counts, latencies (min/max/avg/median/p95/p99)
- **WebSocket Logs**: Collect Memgraph logs via WebSocket
- **Query Mode**: Execute single Cypher queries
- **Resilient**: Automatically retries connection on failures
- **Containerized**: Includes Dockerfile for easy deployment
- **Kubernetes Ready**: Deployment manifests for flexible usage

## Configuration

Environment variables:
- `NEO4J_USERNAME`: Username for authentication (default: `memgraph`)
- `NEO4J_PASSWORD`: Password for authentication (default: empty)
- `WRITE_INTERVAL`: Milliseconds between writes (default: `1000`)

## Usage

```bash
# Install dependencies
pip install -r requirements.txt

# Default: write to memgraph-controller:7687
python client.py

# Write to specific address
python client.py write bolt://memgraph-ha-0:7687

# Execute single query
python client.py query bolt://memgraph-ha-0:7687 "SHOW REPLICATION ROLE"

# Collect WebSocket logs
python client.py logs ws://10.244.0.5:7444 /tmp/logs.jsonl

# Idle mode (sleep forever)
python client.py idle

# Run with faster writes
WRITE_INTERVAL=500 python client.py write bolt://memgraph-ha-0:7687
```

## Docker Build & Run

```bash
# Build image
docker build -t test-client .

# Run container in write mode
docker run --rm -it test-client write bolt://host.docker.internal:7687

# Run with custom settings
docker run --rm -it \
  -e WRITE_INTERVAL=500 \
  test-client write bolt://memgraph.example.com:7687

# Run in idle mode
docker run --rm -it test-client idle
```

## Kubernetes Deployment

```bash
# Build and load image (for local testing with kind/minikube)
docker build -t test-client .
kind load docker-image test-client  # for kind
# or
minikube image load test-client     # for minikube

# Deploy using helm charts
helm install test-client tests/client/charts/test-client

# Check logs
kubectl logs -n memgraph deployment/test-client -f

# Deploy in sandbox mode (idle)
helm install sandbox tests/memgraph-sandbox

# Delete deployments
helm uninstall test-client
```

## Metrics Output

The client logs comprehensive metrics after each write:

```json
{
  "total": 150,
  "success": 148,
  "errors": 2,
  "errorRate": "1.33%",
  "uptime": "150s",
  "latency": {
    "min": 5,
    "max": 245,
    "avg": 23,
    "median": 18,
    "p95": 67,
    "p99": 198
  }
}
```

## Data Schema

Each write creates a node with:
```cypher
CREATE (n:ClientData {
  id: "client_1234567890_abc123",
  timestamp: "2024-01-01T12:00:00.000Z",
  value: "data_1234567890",
  created_at: datetime()
})
```

## Monitoring Queries

View recent writes:
```cypher
MATCH (n:ClientData)
RETURN n
ORDER BY n.created_at DESC
LIMIT 10;
```

Count total writes:
```cypher
MATCH (n:ClientData)
RETURN COUNT(n) as total_writes;
```

Clean up test data:
```cypher
MATCH (n:ClientData)
DETACH DELETE n;
```