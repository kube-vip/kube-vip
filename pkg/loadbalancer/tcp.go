package loadbalancer

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/thebsdbox/kube-vip/pkg/kubevip"
)

// StartTCP a TCP load balancer server instane
func StartTCP(lb *kubevip.LoadBalancer, address string) error {
	log.Infof("Starting TCP Load Balancer for service [%s]", lb.Name)

	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", address, lb.Port))
	if err != nil {
		log.Fatal("listen error:", err)
	}

	for {
		fd, err := l.Accept()
		if err != nil {
			log.Fatal("accept error:", err)
		}
		go persistentConnection(fd, lb)
	}
}

// user -> [LB]
// [LB] (select end pot) -> [endpoint]
//
//
//
//
//
//

func processRequests(lb *kubevip.LoadBalancer, frontendConnection net.Conn) {
	for {
		// READ FROM client
		buf := make([]byte, 1024*1024)
		datalen, err := frontendConnection.Read(buf)
		if err != nil {
			log.Fatalf("%v", err)
		}
		log.Debugf("Sent [%d] bytes to the LB", datalen)
		data := buf[0:datalen]

		// Connect to Endpoint
		ep := lb.ReturnEndpointAddr()

		log.Debugf("Attempting endpoint [%s]", ep)

		endpoint, err := net.Dial("tcp", ep)
		if err != nil {
			fmt.Println("dial error:", err)
			// return nil, err
		}
		log.Debugf("succesfully connected to [%s]", ep)

		// Set a timeout
		endpoint.SetReadDeadline(time.Now().Add(time.Second * 1))

		b, err := endpointRequest(endpoint, ep, string(data))

		_, err = frontendConnection.Write(b)
		if err != nil {
			log.Fatal("Write: ", err)
		}
	}
}

// endpointRequest will take an endpoint address and send the data and wait for the response
func endpointRequest(endpoint net.Conn, endpointAddr, request string) ([]byte, error) {

	// defer conn.Close()
	datalen, err := fmt.Fprintf(endpoint, request)
	if err != nil {
		fmt.Println("dial error:", err)
		return nil, err
	}
	log.Debugf("Sent [%d] bytes to the endpoint", datalen)

	var b bytes.Buffer
	io.Copy(&b, endpoint)
	log.Debugf("Recieved [%d] from the endpoint", b.Len())
	return b.Bytes(), nil
}
