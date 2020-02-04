package loadbalancer

import (
	"io"
	"net"

	log "github.com/sirupsen/logrus"
	"github.com/thebsdbox/kube-vip/pkg/kubevip"
)

// 1. Load balancer port is exposed
// 2. We listen
// 3. On connection we connect to an endpoint
// [loop]
// 4. We read from the load balancer port
// 5. We write traffic to the endpoint
// 6. We read response from endpoint
// 7. We write response to load balancer
// [goto loop]

func persistentConnection(frontendConnection net.Conn, lb *kubevip.LoadBalancer) error {

	var endpoint net.Conn
	for {
		// Connect to Endpoint
		ep := lb.ReturnEndpointAddr()

		log.Debugf("Attempting endpoint [%s]", ep)

		var err error
		endpoint, err = net.Dial("tcp", ep)
		if err != nil {
			log.Warnf("%v", err)
		} else {
			log.Debugf("succesfully connected to [%s]", ep)
			break
		}
	}

	// Build endpoint <front end> connectivity
	go func() { io.Copy(frontendConnection, endpoint) }()
	go func() { io.Copy(endpoint, frontendConnection) }()

	return nil
}
