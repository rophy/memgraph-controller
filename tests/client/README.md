# Neo4j Client with Metrics

A Node.js client for Neo4j/Memgraph that continuously writes data and tracks performance metrics.

## Features

- **Continuous Write Loop**: Writes data to Neo4j/Memgraph at configurable intervals
- **Comprehensive Metrics**: Tracks success/error counts, latencies (min/max/avg/median/p95/p99)
- **Resilient**: Automatically retries connection on failures
- **Containerized**: Includes Dockerfile for easy deployment
- **Kubernetes Ready**: Deployment manifests for single and multi-instance deployments

## Configuration

Environment variables:
- `NEO4J_URI`: Neo4j/Memgraph Bolt URI (default: `bolt://localhost:7687`)
- `NEO4J_USERNAME`: Username for authentication (optional)
- `NEO4J_PASSWORD`: Password for authentication (optional)
- `WRITE_INTERVAL`: Milliseconds between writes (default: `1000`)

## Local Development

```bash
# Install dependencies
npm install

# Run locally (connect to localhost:7687)
npm start

# Run with custom server
NEO4J_URI=bolt://192.168.1.100:7687 npm start

# Run with faster writes
WRITE_INTERVAL=500 npm start
```

## Docker Build & Run

```bash
# Build image
docker build -t neo4j-client .

# Run container (connect to host network)
docker run --rm -it \
  -e NEO4J_URI=bolt://host.docker.internal:7687 \
  neo4j-client

# Run with custom settings
docker run --rm -it \
  -e NEO4J_URI=bolt://memgraph.example.com:7687 \
  -e WRITE_INTERVAL=500 \
  neo4j-client
```

## Kubernetes Deployment

```bash
# Build and load image (for local testing with kind/minikube)
docker build -t neo4j-client .
kind load docker-image neo4j-client  # for kind
# or
minikube image load neo4j-client     # for minikube

# Deploy single instance
kubectl apply -f deployment.yaml

# Check logs
kubectl logs -n memgraph deployment/neo4j-client -f

# Deploy load test (3 parallel clients)
kubectl apply -f deployment.yaml  # includes neo4j-client-loadtest

# Scale load test
kubectl scale deployment neo4j-client-loadtest -n memgraph --replicas=10

# Delete deployments
kubectl delete -f deployment.yaml
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