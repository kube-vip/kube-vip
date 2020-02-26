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
func (lb *LBInstance) startTCP(bindAddress string) error {
	fullAddress := fmt.Sprintf("%s:%d", bindAddress, lb.instance.Port)
	log.Infof("Starting TCP Load Balancer for service [%s]", fullAddress)

	//	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bindAddress, lb.instance.Port))
	laddr, err := net.ResolveTCPAddr("tcp", fullAddress)
	if nil != err {
		log.Errorln(err)
	}
	l, err := net.ListenTCP("tcp", laddr)
	if nil != err {
		return fmt.Errorf("Unable to bind [%s]", err.Error())
	}
	// a:= net.ListenTCP()
	go func() {
		for {
			select {

			case <-lb.stop:
				log.Debugln("Closing listener")

				// We've closed the stop channel
				err = l.Close()
				if err != nil {
					return
					//log.Errorf("Error closing listener [%s]", err)
				}
				// Close the stopped channel as the listener has been stopped
				//lb.mux.Lock()
				close(lb.stopped)
				//lb.mux.Unlock()
				//return
			default:

				err = l.SetDeadline(time.Now().Add(200 * time.Millisecond))
				if err != nil {
					log.Errorf("Error setting TCP deadline", err)
				}
				fd, err := l.Accept()
				if err != nil {
					// Check it it's an accept timeout
					if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
						continue
					} else if err != io.EOF {
						log.Errorf("TCP Accept error [%s]", err)
					}
				}
				log.Debugf("Accepted from [%s]", fd.RemoteAddr())
				//go persistentConnection(fd, lb.instance)
				go processRequests(lb.instance, fd)
			}
		}
	}()
	log.Infof("Load Balancer [%s] started", lb.instance.Name)

	return nil
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
		ep, err := lb.ReturnEndpointAddr()
		if err != nil {
			log.Errorf("No Backends available")
		}
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
