package k8s

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
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
	return newClientset(configPath, inCluster, hostname, time.Second*10)
}

func newClientset(configPath string, inCluster bool, hostname string, timeout time.Duration) (*kubernetes.Clientset, error) {
	config, err := restConfig(configPath, inCluster, timeout)
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

func FindWorkingKubernetesAddress(configPath string, inCluster bool) (*kubernetes.Clientset, error) {
	// check with loopback, and retrieve its certificate
	ips, err := findAddressFromRemoteCert("127.0.0.1:6443")
	if err != nil {
		return nil, err
	}
	for x := range ips {
		log.Debugf("[k8s client] checking with IP address [%s]", ips[x].String())

		k, err := newClientset(configPath, inCluster, net.JoinHostPort(ips[x].String(), "6443"), time.Second*2)
		if err != nil {
			log.Info(err)
		}
		_, err = k.DiscoveryClient.ServerVersion()
		if err == nil {
			log.Infof("[k8s client] working with IP address [%s]", ips[x].String())
			return NewClientset(configPath, inCluster, net.JoinHostPort(ips[x].String(), "6443"))
		}
	}
	return nil, fmt.Errorf("unable to find a working address for the local API server [%v]", err)
}
