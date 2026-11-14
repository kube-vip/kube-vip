package kubevip

import (
	v1 "k8s.io/api/core/v1"
)

// UsesServiceElection reports whether service is under per-service leader
// election: every Service with svc_election, or, with
// per_service_election_on_demand, a Service annotated
// kube-vip.io/forcePerServiceElection: "true". Such a Service's datapath
// belongs to the election winner alone: the winner programs it when it starts
// leading, and the endpoint watcher must not program it on every node with a
// local endpoint.
func UsesServiceElection(config *Config, service *v1.Service) bool {
	if config.EnableServicesElection {
		return true
	}
	return config.PerServiceElectionOnDemand && service != nil &&
		service.Annotations[ForcePerServiceElection] == "true"
}
