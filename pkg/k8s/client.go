package k8s

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClientset takes an optional configPath and creates a new clientset.
// If the configPath is not specified, and inCluster is true, then an
// InClusterConfig is used.
// Also takes a hostname which allow for overriding the config's hostname
// before generating a client.
func NewClientset(configPath string, inCluster bool, hostname string) (*kubernetes.Clientset, error) {
	config, err := restConfig(configPath, inCluster)
	if err != nil {
		panic(err.Error())
	}

	if len(hostname) > 0 {
		config.Host = hostname
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes client: %s", err.Error())
	}
	return clientset, nil
}

func restConfig(kubeconfig string, inCluster bool) (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()

	if kubeconfig != "" && !inCluster {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	if err != nil {
		return nil, err
	}

	// Override some of the defaults allowing a little bit more flexibility speaking with the API server
	// these should hopefully be redundant, however issues will still be logged.
	cfg.QPS = 100
	cfg.Burst = 250
	return cfg, nil
}
