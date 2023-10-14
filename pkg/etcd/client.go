package etcd

import (
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
)

func NewClient(c *kubevip.Config) (*clientv3.Client, error) {
	tlsInfo := transport.TLSInfo{
		TrustedCAFile: c.Etcd.CAFile,
		CertFile:      c.Etcd.ClientCertFile,
		KeyFile:       c.Etcd.ClientKeyFile,
	}

	clientTLS, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}

	return clientv3.New(clientv3.Config{
		Endpoints: c.Etcd.Endpoints,
		TLS:       clientTLS,
	})
}
