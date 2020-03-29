package loadbalancer

import (
	"fmt"

	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
)

//LBInstance - manages the state of load balancer instances
type LBInstance struct {
	stop     chan bool             // Asks LB to stop
	stopped  chan bool             // LB is stopped
	instance *kubevip.LoadBalancer // pointer to a LB instance
	//	mux      sync.Mutex
}

//LBManager - will manage a number of load blancer instances
type LBManager struct {
	loadBalancer []LBInstance
}

//Add - handles the building of the load balancers
func (lm *LBManager) Add(bindAddress string, lb *kubevip.LoadBalancer) error {
	newLB := LBInstance{
		stop:     make(chan bool, 1),
		stopped:  make(chan bool, 1),
		instance: lb,
	}

	switch lb.Type {
	case "tcp":
		err := newLB.startTCP(bindAddress)
		if err != nil {
			return err
		}
	case "udp":
		err := newLB.startUDP(bindAddress)
		if err != nil {
			return err
		}
	case "http":
		err := newLB.startHTTP(bindAddress)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("Unknown Load Balancer type [%s]", lb.Type)
	}

	lm.loadBalancer = append(lm.loadBalancer, newLB)
	return nil
}

//StopAll - handles the building of the load balancers
func (lm *LBManager) StopAll() error {
	log.Debugf("Stopping [%d] loadbalancer instances", len(lm.loadBalancer))
	for x := range lm.loadBalancer {
		err := lm.loadBalancer[x].Stop()
		if err != nil {
			return err
		}
	}
	// Reset the loadbalancer entries
	lm.loadBalancer = nil
	return nil
}

//Stop - handles the building of the load balancers
func (l *LBInstance) Stop() error {

	close(l.stop)

	<-l.stopped
	log.Infof("Load Balancer instance [%s] has stopped", l.instance.Name)
	return nil
}
