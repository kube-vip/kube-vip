package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// Service / VIP Lifecycle
	ActiveServices = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "kube_vip_active_services", Help: "How many LB services are currently managed"},
		[]string{"namespace"},
	)
	ServiceReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "kube_vip_service_reconcile_errors_total", Help: "Count reconcile failures"},
		[]string{"namespace", "name", "reason"},
	)
	ServiceReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "kube_vip_service_reconcile_duration_seconds", Help: "How long AddOrModify takes end-to-end"},
		[]string{"namespace"},
	)

	// This is a prometheus counter used to count the number of events received
	// from the service watcher
	CountServiceWatchEvent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "kube_vip",
		Subsystem: "manager",
		Name:      "all_services_events",
		Help:      "Count all events fired by the service watcher categorised by event type",
	}, []string{"type"})

	// Leader Election
	LeaderTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "kube_vip_leader_election_transitions_total", Help: "Frequent transitions mean instability"},
		[]string{"lease_name"},
	)
	IsLeader = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "kube_vip_is_leader", Help: "1 if this node currently holds the lease"},
		[]string{"node", "lease_name"},
	)
	ServiceElectionLoops = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "kube_vip_service_election_loops",
			Help: "Live per-service leader election restart loops on this node; more than 1 per service means loops leaked"},
		[]string{"namespace", "name"},
	)
	ServiceElectionAttemptsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "kube_vip_service_election_attempts_total",
			Help: "Election attempts made by the per-service leader election restart loop"},
		[]string{"namespace", "name"},
	)
	ServiceElectionErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "kube_vip_service_election_errors_total",
			Help: "Per-service leader election failures by reason"},
		[]string{"namespace", "name", "reason"},
	)

	// This is a prometheus gauge indicating the state of the sessions.
	// 1 means "ESTABLISHED", 0 means "NOT ESTABLISHED"
	BGPSessionInfoGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "kube_vip",
		Subsystem: "manager",
		Name:      "bgp_session_info",
		Help:      "Display state of session by setting metric for label value with current state to 1",
	}, []string{"state", "peer"},
	)

	// General Health
	BuildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "kube_vip_build_info", Help: "Constant 1; track version skew across nodes"},
		[]string{"version", "build", "node"},
	)
)

func RegisterPrometheusMetrics() {
	// Register all metrics with Prometheus
	prometheus.MustRegister(
		ActiveServices,
		ServiceReconcileErrorsTotal,
		ServiceReconcileDuration,
		LeaderTransitionsTotal,
		IsLeader,
		ServiceElectionLoops,
		ServiceElectionAttemptsTotal,
		ServiceElectionErrorsTotal,
		BGPSessionInfoGauge,
		BuildInfo,
		CountServiceWatchEvent,
	)
}
