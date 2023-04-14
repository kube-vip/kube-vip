//nolint:unparam

package service

import "github.com/prometheus/client_golang/prometheus"

// PrometheusCollector - required for statistics // TODO - improve monitoring
func (sm *Manager) PrometheusCollector() []prometheus.Collector {
	return []prometheus.Collector{sm.countServiceWatchEvent}
}
