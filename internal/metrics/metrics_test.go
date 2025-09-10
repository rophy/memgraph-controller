package metrics

import (
  "testing"
  "time"

  "github.com/prometheus/client_golang/prometheus"
  "github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordReconciliation(t *testing.T) {
  registry := prometheus.NewRegistry()
  m := NewWithRegistry(registry)

  // Test successful reconciliation
  m.RecordReconciliation(true, 0.5)
  
  if got := testutil.ToFloat64(m.ReconciliationsTotal); got != 1 {
    t.Errorf("ReconciliationsTotal = %f, want 1", got)
  }
  if got := testutil.ToFloat64(m.ReconciliationsSuccessful); got != 1 {
    t.Errorf("ReconciliationsSuccessful = %f, want 1", got)
  }
  if got := testutil.ToFloat64(m.ReconciliationsFailed); got != 0 {
    t.Errorf("ReconciliationsFailed = %f, want 0", got)
  }

  // Test failed reconciliation
  m.RecordReconciliation(false, 1.2)
  
  if got := testutil.ToFloat64(m.ReconciliationsTotal); got != 2 {
    t.Errorf("ReconciliationsTotal = %f, want 2", got)
  }
  if got := testutil.ToFloat64(m.ReconciliationsSuccessful); got != 1 {
    t.Errorf("ReconciliationsSuccessful = %f, want 1", got)
  }
  if got := testutil.ToFloat64(m.ReconciliationsFailed); got != 1 {
    t.Errorf("ReconciliationsFailed = %f, want 1", got)
  }
}

func TestUpdateClusterState(t *testing.T) {
  registry := prometheus.NewRegistry()
  m := NewWithRegistry(registry)

  // Test healthy cluster
  m.UpdateClusterState(true, 3, 3, true)
  
  if got := testutil.ToFloat64(m.ClusterState); got != 1 {
    t.Errorf("ClusterState = %f, want 1", got)
  }
  if got := testutil.ToFloat64(m.ClusterPodsTotal); got != 3 {
    t.Errorf("ClusterPodsTotal = %f, want 3", got)
  }
  if got := testutil.ToFloat64(m.ClusterPodsHealthy); got != 3 {
    t.Errorf("ClusterPodsHealthy = %f, want 3", got)
  }
  if got := testutil.ToFloat64(m.ClusterSyncReplicaHealthy); got != 1 {
    t.Errorf("ClusterSyncReplicaHealthy = %f, want 1", got)
  }

  // Test unhealthy cluster
  m.UpdateClusterState(false, 3, 2, false)
  
  if got := testutil.ToFloat64(m.ClusterState); got != 0 {
    t.Errorf("ClusterState = %f, want 0", got)
  }
  if got := testutil.ToFloat64(m.ClusterPodsHealthy); got != 2 {
    t.Errorf("ClusterPodsHealthy = %f, want 2", got)
  }
  if got := testutil.ToFloat64(m.ClusterSyncReplicaHealthy); got != 0 {
    t.Errorf("ClusterSyncReplicaHealthy = %f, want 0", got)
  }
}

func TestRecordMainChange(t *testing.T) {
  registry := prometheus.NewRegistry()
  m := NewWithRegistry(registry)

  m.RecordMainChange()
  if got := testutil.ToFloat64(m.ClusterMainChanges); got != 1 {
    t.Errorf("ClusterMainChanges = %f, want 1", got)
  }

  m.RecordMainChange()
  if got := testutil.ToFloat64(m.ClusterMainChanges); got != 2 {
    t.Errorf("ClusterMainChanges = %f, want 2", got)
  }
}

func TestGatewayMetrics(t *testing.T) {
  registry := prometheus.NewRegistry()
  m := NewWithRegistry(registry)

  // Test connection metrics
  m.UpdateGatewayConnections(5)
  if got := testutil.ToFloat64(m.GatewayConnectionsActive); got != 5 {
    t.Errorf("GatewayConnectionsActive = %f, want 5", got)
  }

  m.RecordGatewayConnection()
  if got := testutil.ToFloat64(m.GatewayConnectionsTotal); got != 1 {
    t.Errorf("GatewayConnectionsTotal = %f, want 1", got)
  }

  // Test bytes transferred
  m.RecordGatewayBytes(1024, 2048)
  if got := testutil.ToFloat64(m.GatewayBytesTransferred.WithLabelValues("sent")); got != 1024 {
    t.Errorf("GatewayBytesTransferred[sent] = %f, want 1024", got)
  }
  if got := testutil.ToFloat64(m.GatewayBytesTransferred.WithLabelValues("received")); got != 2048 {
    t.Errorf("GatewayBytesTransferred[received] = %f, want 2048", got)
  }

  // Test errors
  m.RecordGatewayError()
  if got := testutil.ToFloat64(m.GatewayConnectionErrors); got != 1 {
    t.Errorf("GatewayConnectionErrors = %f, want 1", got)
  }
}

func TestLeadershipMetrics(t *testing.T) {
  registry := prometheus.NewRegistry()
  m := NewWithRegistry(registry)

  // Test leadership status
  m.UpdateLeadershipStatus(true)
  if got := testutil.ToFloat64(m.ControllerIsLeader); got != 1 {
    t.Errorf("ControllerIsLeader = %f, want 1", got)
  }

  m.UpdateLeadershipStatus(false)
  if got := testutil.ToFloat64(m.ControllerIsLeader); got != 0 {
    t.Errorf("ControllerIsLeader = %f, want 0", got)
  }

  // Test leadership changes
  m.RecordLeadershipChange()
  if got := testutil.ToFloat64(m.ControllerLeadershipChanges); got != 1 {
    t.Errorf("ControllerLeadershipChanges = %f, want 1", got)
  }

  // Test elections
  m.RecordElection()
  if got := testutil.ToFloat64(m.ControllerElections); got != 1 {
    t.Errorf("ControllerElections = %f, want 1", got)
  }
}

func TestErrorMetrics(t *testing.T) {
  registry := prometheus.NewRegistry()
  m := NewWithRegistry(registry)

  // Record different error types
  m.RecordError("reconciliation")
  m.RecordError("reconciliation")
  m.RecordError("memgraph_query")
  m.RecordError("k8s_api")

  if got := testutil.ToFloat64(m.ErrorsTotal.WithLabelValues("reconciliation")); got != 2 {
    t.Errorf("ErrorsTotal[reconciliation] = %f, want 2", got)
  }
  if got := testutil.ToFloat64(m.ErrorsTotal.WithLabelValues("memgraph_query")); got != 1 {
    t.Errorf("ErrorsTotal[memgraph_query] = %f, want 1", got)
  }
  if got := testutil.ToFloat64(m.ErrorsTotal.WithLabelValues("k8s_api")); got != 1 {
    t.Errorf("ErrorsTotal[k8s_api] = %f, want 1", got)
  }

  // Check that last error timestamp is set (should be close to current time)
  now := float64(time.Now().Unix())
  if got := testutil.ToFloat64(m.LastErrorTimestamp.WithLabelValues("reconciliation")); got < now-1 || got > now+1 {
    t.Errorf("LastErrorTimestamp[reconciliation] = %f, want close to %f", got, now)
  }
}