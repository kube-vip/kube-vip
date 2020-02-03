package loadbalancer

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/thebsdbox/llb/pkg/config"
)

// Start a TCP load balancer server
func StartTCP() {
	log.Infoln("Starting any tcp load balancing services")
	for i := range config.Services {
		if config.Services[i].ServiceType == "tcp" {
			log.Debugf("Starting service [%s]", config.Services[i].Name)
			startListener(&config.Services[i])
		}
	}
}

func startListener(s *config.Service) error {
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.Address(), s.Port()))
	if err != nil {
		log.Fatal("listen error:", err)
	}

	for {
		fd, err := l.Accept()
		if err != nil {
			log.Fatal("accept error:", err)
		}
		go persistentConnection(fd, s)
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

func processRequests(frontendConnection net.Conn, s *config.Service) {
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
		ep := s.ReturnEndpointAddr()

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
