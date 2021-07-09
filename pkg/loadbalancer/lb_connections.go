package loadbalancer

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/kube-vip/kube-vip/pkg/kubevip"
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

func persistentConnection(frontendConnection net.Conn, lb *kubevip.LoadBalancer) {

	var endpoint net.Conn
	defer frontendConnection.Close()
	// Makes sure we close the connections to the endpoint when we've completed

	// Set a timeout for connecting to an endpoint
	dialer := net.Dialer{Timeout: time.Millisecond * 500}
	for {

		// Connect to Endpoint
		ep, err := lb.ReturnEndpointAddr()
		if err != nil {
			return
		}

		// We now dial to an endpoint with a timeout of half a second
		// TODO - make this adjustable
		endpoint, err = dialer.Dial("tcp", ep)
		if err != nil {
			log.Debugf("%v", err)
			log.Warnf("[%s]---X [FAILED] X-->[%s]", frontendConnection.RemoteAddr(), ep)
		} else {
			log.Debugf("[%s]---->[ACCEPT]---->[%s]", frontendConnection.RemoteAddr(), ep)
			defer endpoint.Close()
			break
		}
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)

	// Begin copying incoming (frontend -> to an endpoint)
	go func() {
		bytes, err := io.Copy(endpoint, frontendConnection)
		log.Debugf("[%d] bytes of data sent to endpoint", bytes)
		if err != nil {
			log.Warnf("Error sending data to endpoint [%s] [%v]", endpoint.RemoteAddr(), err)
		}
		wg.Done()
	}()
	// go func() {
	// Begin copying recieving (endpoint -> back to frontend)
	bytes, err := io.Copy(frontendConnection, endpoint)
	log.Debugf("[%d] bytes of data sent to client", bytes)
	if err != nil {
		log.Warnf("Error sending data to frontend [%s] [%s]", frontendConnection.RemoteAddr(), err)
	}
	// 	wg.Done()
	// 	endpoint.Close()
	// }()
	wg.Wait()
}

func persistentUDPConnection(frontendConnection net.Conn, lb *kubevip.LoadBalancer) {

	var endpoint net.Conn
	defer frontendConnection.Close()
	// Makes sure we close the connections to the endpoint when we've completed

	// Set a timeout for connecting to an endpoint
	dialer := net.Dialer{Timeout: time.Millisecond * 500}
	for {

		// Connect to Endpoint
		ep, err := lb.ReturnEndpointAddr()
		if err != nil {
			return
		}

		// We now dial to an endpoint with a timeout of half a second
		// TODO - make this adjustable
		endpoint, err = dialer.Dial("udp", ep)
		if err != nil {
			log.Debugf("%v", err)
			log.Warnf("[%s]---X [FAILED] X-->[%s]", frontendConnection.RemoteAddr(), ep)
		} else {
			log.Debugf("[%s]---->[ACCEPT]---->[%s]", frontendConnection.RemoteAddr(), ep)
			defer endpoint.Close()
			break
		}
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)

	// Begin copying incoming (frontend -> to an endpoint)
	go func() {
		bytes, err := io.Copy(endpoint, frontendConnection)
		log.Debugf("[%d] bytes of data sent to endpoint", bytes)
		if err != nil {
			log.Warnf("Error sending data to endpoint [%s] [%v]", endpoint.RemoteAddr(), err)
		}
		wg.Done()
	}()
	// go func() {
	// Begin copying recieving (endpoint -> back to frontend)
	bytes, err := io.Copy(frontendConnection, endpoint)
	log.Debugf("[%d] bytes of data sent to client", bytes)
	if err != nil {
		log.Warnf("Error sending data to frontend [%s] [%s]", frontendConnection.RemoteAddr(), err)
	}
	// 	wg.Done()
	// 	endpoint.Close()
	// }()
	wg.Wait()
}
