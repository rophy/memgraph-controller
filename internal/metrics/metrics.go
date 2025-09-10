package metrics

import (
  "github.com/prometheus/client_golang/prometheus"
  "github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the memgraph-controller
type Metrics struct {
  // Reconciliation metrics
  ReconciliationsTotal      prometheus.Counter
  ReconciliationsSuccessful prometheus.Counter
  ReconciliationsFailed     prometheus.Counter
  ReconciliationDuration    prometheus.Histogram
  LastReconciliationTime    prometheus.Gauge

  // Cluster state metrics
  ClusterState          prometheus.Gauge
  ClusterMainChanges    prometheus.Counter
  ClusterSyncReplicaHealthy prometheus.Gauge
  ClusterPodsTotal      prometheus.Gauge
  ClusterPodsHealthy    prometheus.Gauge

  // Gateway connection metrics
  GatewayConnectionsActive prometheus.Gauge
  GatewayConnectionsTotal  prometheus.Counter
  GatewayBytesTransferred  *prometheus.CounterVec
  GatewayConnectionErrors  prometheus.Counter

  // Leadership metrics
  ControllerIsLeader       prometheus.Gauge
  ControllerLeadershipChanges prometheus.Counter
  ControllerElections      prometheus.Counter

  // Error metrics
  ErrorsTotal        *prometheus.CounterVec
  LastErrorTimestamp *prometheus.GaugeVec
}

// New creates and registers all Prometheus metrics with the default registry
func New() *Metrics {
  return NewWithRegistry(prometheus.DefaultRegisterer)
}

// NewWithRegistry creates and registers all Prometheus metrics with a custom registry
func NewWithRegistry(reg prometheus.Registerer) *Metrics {
  factory := promauto.With(reg)
  m := &Metrics{
    // Reconciliation metrics
    ReconciliationsTotal: factory.NewCounter(prometheus.CounterOpts{
      Name: "memgraph_controller_reconciliations_total",
      Help: "Total number of reconciliation attempts",
    }),
    ReconciliationsSuccessful: factory.NewCounter(prometheus.CounterOpts{
      Name: "memgraph_controller_reconciliations_successful_total",
      Help: "Total number of successful reconciliations",
    }),
    ReconciliationsFailed: factory.NewCounter(prometheus.CounterOpts{
      Name: "memgraph_controller_reconciliations_failed_total",
      Help: "Total number of failed reconciliations",
    }),
    ReconciliationDuration: factory.NewHistogram(prometheus.HistogramOpts{
      Name:    "memgraph_controller_reconciliation_duration_seconds",
      Help:    "Duration of reconciliation operations in seconds",
      Buckets: prometheus.DefBuckets,
    }),
    LastReconciliationTime: factory.NewGauge(prometheus.GaugeOpts{
      Name: "memgraph_controller_last_reconciliation_timestamp",
      Help: "Unix timestamp of the last reconciliation",
    }),

    // Cluster state metrics
    ClusterState: factory.NewGauge(prometheus.GaugeOpts{
      Name: "memgraph_cluster_state",
      Help: "Current cluster state (1=healthy, 0=unhealthy)",
    }),
    ClusterMainChanges: factory.NewCounter(prometheus.CounterOpts{
      Name: "memgraph_cluster_main_changes_total",
      Help: "Total number of main node changes",
    }),
    ClusterSyncReplicaHealthy: factory.NewGauge(prometheus.GaugeOpts{
      Name: "memgraph_cluster_sync_replica_healthy",
      Help: "Sync replica health status (1=healthy, 0=unhealthy)",
    }),
    ClusterPodsTotal: factory.NewGauge(prometheus.GaugeOpts{
      Name: "memgraph_cluster_pods_total",
      Help: "Total number of pods in the cluster",
    }),
    ClusterPodsHealthy: factory.NewGauge(prometheus.GaugeOpts{
      Name: "memgraph_cluster_pods_healthy",
      Help: "Number of healthy pods in the cluster",
    }),

    // Gateway connection metrics
    GatewayConnectionsActive: factory.NewGauge(prometheus.GaugeOpts{
      Name: "memgraph_gateway_connections_active",
      Help: "Number of currently active gateway connections",
    }),
    GatewayConnectionsTotal: factory.NewCounter(prometheus.CounterOpts{
      Name: "memgraph_gateway_connections_total",
      Help: "Total number of gateway connections handled",
    }),
    GatewayBytesTransferred: factory.NewCounterVec(
      prometheus.CounterOpts{
        Name: "memgraph_gateway_bytes_transferred_total",
        Help: "Total bytes transferred through the gateway",
      },
      []string{"direction"}, // "sent" or "received"
    ),
    GatewayConnectionErrors: factory.NewCounter(prometheus.CounterOpts{
      Name: "memgraph_gateway_connection_errors_total",
      Help: "Total number of gateway connection errors",
    }),

    // Leadership metrics
    ControllerIsLeader: factory.NewGauge(prometheus.GaugeOpts{
      Name: "memgraph_controller_is_leader",
      Help: "Controller leadership status (1=leader, 0=follower)",
    }),
    ControllerLeadershipChanges: factory.NewCounter(prometheus.CounterOpts{
      Name: "memgraph_controller_leadership_changes_total",
      Help: "Total number of leadership changes",
    }),
    ControllerElections: factory.NewCounter(prometheus.CounterOpts{
      Name: "memgraph_controller_elections_total",
      Help: "Total number of leader elections",
    }),

    // Error metrics
    ErrorsTotal: factory.NewCounterVec(
      prometheus.CounterOpts{
        Name: "memgraph_controller_errors_total",
        Help: "Total number of errors by type",
      },
      []string{"type"}, // "reconciliation", "memgraph_query", "k8s_api"
    ),
    LastErrorTimestamp: factory.NewGaugeVec(
      prometheus.GaugeOpts{
        Name: "memgraph_controller_last_error_timestamp",
        Help: "Unix timestamp of the last error by type",
      },
      []string{"type"},
    ),
  }

  return m
}

// RecordReconciliation records a reconciliation attempt
func (m *Metrics) RecordReconciliation(success bool, duration float64) {
  m.ReconciliationsTotal.Inc()
  if success {
    m.ReconciliationsSuccessful.Inc()
  } else {
    m.ReconciliationsFailed.Inc()
  }
  m.ReconciliationDuration.Observe(duration)
  m.LastReconciliationTime.SetToCurrentTime()
}

// UpdateClusterState updates cluster state metrics
func (m *Metrics) UpdateClusterState(healthy bool, totalPods, healthyPods int, syncReplicaHealthy bool) {
  if healthy {
    m.ClusterState.Set(1)
  } else {
    m.ClusterState.Set(0)
  }
  m.ClusterPodsTotal.Set(float64(totalPods))
  m.ClusterPodsHealthy.Set(float64(healthyPods))
  if syncReplicaHealthy {
    m.ClusterSyncReplicaHealthy.Set(1)
  } else {
    m.ClusterSyncReplicaHealthy.Set(0)
  }
}

// RecordMainChange records a main node change
func (m *Metrics) RecordMainChange() {
  m.ClusterMainChanges.Inc()
}

// UpdateGatewayConnections updates gateway connection metrics
func (m *Metrics) UpdateGatewayConnections(active int) {
  m.GatewayConnectionsActive.Set(float64(active))
}

// RecordGatewayConnection records a new gateway connection
func (m *Metrics) RecordGatewayConnection() {
  m.GatewayConnectionsTotal.Inc()
}

// RecordGatewayBytes records bytes transferred through the gateway
func (m *Metrics) RecordGatewayBytes(sent, received int64) {
  m.GatewayBytesTransferred.WithLabelValues("sent").Add(float64(sent))
  m.GatewayBytesTransferred.WithLabelValues("received").Add(float64(received))
}

// RecordGatewayError records a gateway connection error
func (m *Metrics) RecordGatewayError() {
  m.GatewayConnectionErrors.Inc()
}

// UpdateLeadershipStatus updates leadership status
func (m *Metrics) UpdateLeadershipStatus(isLeader bool) {
  if isLeader {
    m.ControllerIsLeader.Set(1)
  } else {
    m.ControllerIsLeader.Set(0)
  }
}

// RecordLeadershipChange records a leadership change
func (m *Metrics) RecordLeadershipChange() {
  m.ControllerLeadershipChanges.Inc()
}

// RecordElection records a leader election
func (m *Metrics) RecordElection() {
  m.ControllerElections.Inc()
}

// RecordError records an error occurrence
func (m *Metrics) RecordError(errorType string) {
  m.ErrorsTotal.WithLabelValues(errorType).Inc()
  m.LastErrorTimestamp.WithLabelValues(errorType).SetToCurrentTime()
}