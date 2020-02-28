package loadbalancer

import (
	"io"
	"net"

	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
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
		ep, err := lb.ReturnEndpointAddr()
		if err != nil {
			return err
		}

		endpoint, err = net.Dial("tcp", ep)
		if err != nil {
			log.Debugf("%v", err)
			log.Warnf("[%s]---X [FAILED] X-->[%s]", frontendConnection.RemoteAddr(), ep)
		} else {
			log.Debugf("[%s]---->[ACCEPT]---->[%s]", frontendConnection.RemoteAddr(), ep)
			break
		}
	}

	// Build endpoint <front end> connectivity
	go func() { io.Copy(frontendConnection, endpoint) }()
	go func() { io.Copy(endpoint, frontendConnection) }()

	return nil
}
