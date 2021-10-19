package loadbalancer

import (
	"fmt"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
)

// StartTCP a TCP load balancer server instane
func (lb *LBInstance) startTCP(bindAddress string) error {
	fullAddress := fmt.Sprintf("%s:%d", bindAddress, lb.instance.Port)
	log.Infof("Starting TCP Load Balancer for service [%s]", fullAddress)

	laddr, err := net.ResolveTCPAddr("tcp", fullAddress)
	if nil != err {
		log.Errorln(err)
	}
	l, err := net.ListenTCP("tcp", laddr)
	if nil != err {
		return fmt.Errorf("Unable to bind [%s]", err.Error())
	}
	go func() {
		for {
			select {

			case <-lb.stop:
				log.Debugln("Closing listener")

				// We've closed the stop channel
				err = l.Close()
				if err != nil {
					return
				}
				// Close the stopped channel as the listener has been stopped
				close(lb.stopped)
			default:

				err = l.SetDeadline(time.Now().Add(200 * time.Millisecond))
				if err != nil {
					log.Errorf("Error setting TCP deadline [%v]", err)
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
				go persistentConnection(fd, lb.instance)
			}
		}
	}()
	log.Infof("Load Balancer [%s] started", lb.instance.Name)

	return nil
}
