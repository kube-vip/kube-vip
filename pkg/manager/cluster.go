package manager

import (
	"github.com/pkg/errors"

	"github.com/kube-vip/kube-vip/pkg/cluster"
	"github.com/kube-vip/kube-vip/pkg/etcd"
)

func initClusterManager(sm *Manager) (*cluster.Manager, error) {
	m := &cluster.Manager{
		SignalChan: sm.signalChan,
	}

	switch sm.config.LeaderElectionType {
	case "kubernetes", "":
		m.KubernetesClient = sm.clientSet
		m.RetryWatcherClient = sm.rwClientSet
	case "etcd":
		client, err := etcd.NewClient(sm.config)
		if err != nil {
			return nil, err
		}
		m.EtcdClient = client
	default:
		return nil, errors.Errorf("invalid LeaderElectionMode %s not supported", sm.config.LeaderElectionType)
	}

	return m, nil
}
