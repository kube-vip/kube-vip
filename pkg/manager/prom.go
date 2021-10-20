package manager

import "github.com/prometheus/client_golang/prometheus"

// PrometheusCollector defines a service watch event counter.
func (sm *Manager) PrometheusCollector() []prometheus.Collector {
	return []prometheus.Collector{sm.countServiceWatchEvent}
}
