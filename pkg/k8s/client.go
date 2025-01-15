package k8s

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	log "github.com/gookit/slog"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultTimeout = 10 * time.Second
)

// NewClientset takes REST config and returns k8s clientest.
func NewClientset(config *rest.Config) (*kubernetes.Clientset, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes client: %s", err.Error())
	}
	return clientset, nil
}

// NewRestConfig takes an optional configPath and creates a new REST config for clientset.
// If the configPath is not specified, and inCluster is true, then an
// InClusterConfig is used.
// Also takes a hostname which allow for overriding the config's hostname.
func NewRestConfig(configPath string, inCluster bool, hostname string) (*rest.Config, error) {
	config, err := restConfig(configPath, inCluster, defaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to create rest config: %w", err)
	}

	if len(hostname) > 0 {
		config.Host = hostname
	}

	return config, nil
}

func restConfig(kubeconfig string, inCluster bool, timeout time.Duration) (*rest.Config, error) {
	var cfg *rest.Config
	var err error
	if inCluster {
		if cfg, err = rest.InClusterConfig(); err != nil {
			return nil, fmt.Errorf("failed to get incluster config: %w", err)
		}
	} else if kubeconfig != "" {
		if cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig); err != nil {
			return nil, fmt.Errorf("failed to build config from file '%s': %w", kubeconfig, err)
		}
	} else {
		return nil, fmt.Errorf("failed to build config from file: path to KubeConfig not specified")
	}

	// Override some of the defaults allowing a little bit more flexibility speaking with the API server
	// these should hopefully be redundant, however issues will still be logged.
	cfg.QPS = 100
	cfg.Burst = 250
	cfg.Timeout = timeout
	return cfg, nil
}

func findAddressFromRemoteCert(address string) ([]net.IP, error) {

	// TODO: should we care at this point, probably not as we just want the certificates
	conf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint
	}
	d := &net.Dialer{
		Timeout: time.Duration(3) * time.Second,
	}

	// Create the TCP connection
	conn, err := tls.DialWithDialer(d, "tcp", address, conf)
	if err != nil {
		return nil, err
	}

	defer conn.Close()
	// Grab the certificactes
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) > 1 {
		return nil, fmt.Errorf("[k8s client] not designed to recive multiple certs from API server")
	}

	return certs[0].IPAddresses, nil
}

func FindWorkingKubernetesAddress(configPath string, inCluster bool) (*rest.Config, error) {
	// check with loopback, and retrieve its certificate
	ips, err := findAddressFromRemoteCert("127.0.0.1:6443")
	if err != nil {
		return nil, err
	}
	for x := range ips {
		log.Debugf("[k8s client] checking with IP address [%s]", ips[x].String())
		c, err := NewRestConfig(configPath, inCluster, net.JoinHostPort(ips[x].String(), "6443"))
		if err != nil {
			log.Errorf("failed to create k8s REST config: %v", err)
		}

		c.Timeout = 2 * time.Second
		k, err := NewClientset(c)
		if err != nil {
			log.Errorf("failed to create k8s clientset: %v", err)
		}

		_, err = k.DiscoveryClient.ServerVersion()
		if err == nil {
			log.Infof("[k8s client] working with IP address [%s]", ips[x].String())
			c.Timeout = defaultTimeout
			return c, nil
		}
	}
	return nil, fmt.Errorf("unable to find a working address for the local API server [%v]", err)
}
