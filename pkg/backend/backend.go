package backend

import (
	"fmt"
	"time"

	log "log/slog"

	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Entry struct {
	Addr    string
	Port    uint16
	IsLocal bool
}

type Map map[Entry]bool

func (e *Entry) Check() bool {
	var client *kubernetes.Clientset
	var err error
	var config *rest.Config

	adminConfigPath := "/etc/kubernetes/admin.conf"
	// TODO: add one more switch case of homeConfigPath if there is such scenario in future
	// homeConfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")

	var k8sAddr string
	if vip.IsIPv4(e.Addr) {
		k8sAddr = fmt.Sprintf("%s:%v", e.Addr, e.Port)
	} else {
		k8sAddr = fmt.Sprintf("[%s]:%v", e.Addr, e.Port)
	}

	switch {
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

func Watch(tickAction func(), interval int, stop chan struct{}) {
	if interval <= 0 {
		interval = 5
	}

	ticker := time.NewTicker(time.Second * time.Duration(interval))
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			ticker.Stop()
			return
		case <-ticker.C:
			ticker.Stop()
			tickAction()
			ticker.Reset(time.Second * time.Duration(interval))
		}
	}
}
