package loadbalancer

import "github.com/thebsdbox/kube-vip/pkg/kubevip"

//lbState - manages the state of load balancer instances
type lbState []struct {
	stop       chan bool             // Asks LB to stop
	stopped    chan bool             // LB is stopped
	lbInstance *kubevip.LoadBalancer // pointer to a LB instance
}

func ParseLBS() error {
	return nil
}
