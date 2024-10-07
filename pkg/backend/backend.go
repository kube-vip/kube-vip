package backend

import (
	"fmt"
	"time"

	"github.com/kube-vip/kube-vip/pkg/k8s"
	"github.com/kube-vip/kube-vip/pkg/utils"
	"github.com/kube-vip/kube-vip/pkg/vip"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
)

type Entry struct {
	Addr string
	Port int
}

type Map map[Entry]bool

func (e *Entry) Check() bool {
	var client *kubernetes.Clientset
	var err error

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
		client, err = k8s.NewClientset(adminConfigPath, false, k8sAddr)
		if err != nil {
			log.Infof("could not create k8s clientset from external file: %q: %v", adminConfigPath, err)
			return false
		}
	default:

		client, err = k8s.NewClientset("", true, k8sAddr)
		if err != nil {
			log.Infof("could not create k8s clientset %v", err)
			return false
		}
	}

	_, err = client.DiscoveryClient.ServerVersion()
	if err != nil {
		log.Infof("failed check k8s server version: %s", err)
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
