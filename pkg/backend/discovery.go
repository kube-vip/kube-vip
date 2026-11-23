package backend

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type discoveryBackend struct {
	generic
	kubeConfigPath string
}

func newDiscoveryBackend(config *Config) *discoveryBackend {
	return &discoveryBackend{
		backendType:    Discovery,
		addr:           config.Address,
		port:           config.Port,
		kubeConfigPath: config.KubeConfigPath,
	}
}

func (d *discoveryBackend) createClient() (*kubernetes.Clientset, error) {
	var client *kubernetes.Clientset
	var err error
	var config *rest.Config

	adminConfigPath := "/etc/kubernetes/admin.conf"
	// TODO: add one more switch case of homeConfigPath if there is such scenario in future
	// homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")

	k8sAddr := net.JoinHostPort(d.addr, strconv.Itoa(int(d.port)))

	switch {
	case d.kubeConfigPath != "" && utils.FileExists(d.kubeConfigPath):
		config, err = k8s.NewRestConfig(d.kubeConfigPath, false, k8sAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to create k8s REST config for path %q: %w", d.kubeConfigPath, err)
		}
	case utils.FileExists(adminConfigPath):
		config, err = k8s.NewRestConfig(adminConfigPath, false, k8sAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to create k8s REST config for path %q: %w", adminConfigPath, err)
		}
	default:
		config, err = k8s.NewRestConfig("", true, k8sAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to create k8s REST config: %w", err)
		}
	}

	client, err = k8s.NewClientset(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	return client, nil
}

func (d *discoveryBackend) Check(ctx context.Context) (bool, error) {
	client, err := d.createClient()
	if err != nil {
		return false, err
	}

	_, err = client.DiscoveryClient.ServerVersionWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to discover k8s version: %w", err)
	}
	return true, nil
}
