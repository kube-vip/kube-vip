package backend

import (
	"context"
	"fmt"
	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type discoveryBackend struct {
	generic
}

func newDiscoveryBackend(config *Config) *discoveryBackend {
	return &discoveryBackend{generic{
		addr:           config.Address,
		port:           config.Port,
		kubeConfigPath: config.KubeConfigPath,
		isLocal:        config.IsLocal,
	}}
}

func (d *discoveryBackend) Check(_ context.Context) bool {
	var client *kubernetes.Clientset
	var err error
	var config *rest.Config

	adminConfigPath := "/etc/kubernetes/admin.conf"
	// TODO: add one more switch case of homeConfigPath if there is such scenario in future
	// homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")

	var k8sAddr string
	if utils.IsIPv6(d.addr) {
		k8sAddr = fmt.Sprintf("[%s]:%v", d.addr, d.port)
	} else {
		k8sAddr = fmt.Sprintf("%s:%v", d.addr, d.port)
	}

	switch {
	case d.kubeConfigPath != "" && utils.FileExists(d.kubeConfigPath):
		config, err = k8s.NewRestConfig(d.kubeConfigPath, false, k8sAddr)
		if err != nil {
			log.Error("create k8s REST config", "path", d.kubeConfigPath, "err", err)
			return false
		}
	case utils.FileExists(adminConfigPath):
		config, err = k8s.NewRestConfig(adminConfigPath, false, k8sAddr)
		if err != nil {
			log.Error("create k8s REST config", "path", adminConfigPath, "err", err)
			return false
		}
	default:
		config, err = k8s.NewRestConfig("", true, k8sAddr)
		if err != nil {
			log.Error("create k8s REST config", "err", err)
			return false
		}
	}

	client, err = k8s.NewClientset(config)
	if err != nil {
		log.Error("create k8s client", "err", err)
		return false
	}

	_, err = client.DiscoveryClient.ServerVersion()
	if err != nil {
		log.Error("discover k8s version", "err", err)
		return false
	}
	return true
}
