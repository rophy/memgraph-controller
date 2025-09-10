package metrics

import (
  "net/http"

  "github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler returns an HTTP handler for Prometheus metrics
func Handler() http.Handler {
  return promhttp.Handler()
}