package manager

import "github.com/prometheus/client_golang/prometheus"

// PrometheusCollector defines a service watch event counter.
func (sm *Manager) PrometheusCollector() []prometheus.Collector {
	collectors := []prometheus.Collector{}
	if sm.svcProcessor != nil {
		collectors = append(collectors, sm.svcProcessor.CountServiceWatchEvent)
	}
	if sm.bgpServer != nil {
		collectors = append(collectors, sm.bgpServer.BGPSessionInfoGauge)
	}
	return collectors
}
