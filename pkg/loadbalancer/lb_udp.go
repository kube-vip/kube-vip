package loadbalancer

import (
	"fmt"
	"net"

	log "github.com/sirupsen/logrus"
)

// StartTCP a TCP load balancer server instane
func (lb *LBInstance) startUDP(bindAddress string) error {
	fullAddress := fmt.Sprintf("%s:%d", bindAddress, lb.instance.Port)
	log.Infof("Starting UDP Load Balancer for service [%s]", fullAddress)

	laddr, err := net.ResolveUDPAddr("udp", fullAddress)
	if nil != err {
		log.Errorln(err)
	}
	l, err := net.ListenUDP("udp", laddr)
	if nil != err {
		return fmt.Errorf("Unable to bind [%s]", err.Error())
	}
	//stopListener := make(chan bool)

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

				// err = l.SetDeadline(time.Now().Add(200 * time.Millisecond))
				// if err != nil {
				// 	log.Errorf("Error setting TCP deadline", err)
				// }
				// fd, err := l.Accept()
				// if err != nil {
				// 	// Check it it's an accept timeout
				// 	if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
				// 		continue
				// 	} else if err != io.EOF {
				// 		log.Errorf("TCP Accept error [%s]", err)
				// 	}
				// }
				go persistentUDPConnection(l, lb.instance)
			}
		}
	}()
	log.Infof("Load Balancer [%s] started", lb.instance.Name)

	// go func() {
	// 	<-lb.stop
	// 	log.Debugln("Closing listener")

	// 	// We've closed the stop channel
	// 	err = l.Close()
	// 	if err != nil {
	// 		return
	// 	}
	// 	// Close the stopped channel as the listener has been stopped
	// 	close(stopListener)
	// }()

	// // Create a listener per CPU
	// // TODO - make this customisable?

	// //for i := 0; i < runtime.NumCPU(); i++ {
	// go persistentUDPConnection(l, lb.instance)
	// //}

	//<-stopListener // hang until an error

	log.Infof("Load Balancer [%s] started", lb.instance.Name)

	return nil
}

// func listen(connection *net.UDPConn, quit chan bool) {
// 	buffer := make([]byte, 1024)
// 	n, remoteAddr, err := 0, new(net.UDPAddr), error(nil)
// 	for err == nil {

// 		//TODO -

// 		n, remoteAddr, err = connection.ReadFromUDP(buffer)
// 		// you might copy out the contents of the packet here, to
// 		// `var r myapp.Request`, say, and `go handleRequest(r)` (or
// 		// send it down a channel) to free up the listening
// 		// goroutine. you do *need* to copy then, though,
// 		// because you've only made one buffer per listen().
// 		fmt.Println("from", remoteAddr, "-", buffer[:n])
// 	}
// 	fmt.Println("listener failed - ", err)
// }
