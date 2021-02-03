package manager

import "github.com/prometheus/client_golang/prometheus"

func (sm *Manager) PromethesCollector() []prometheus.Collector {
	return []prometheus.Collector{sm.countServiceWatchEvent}
}
