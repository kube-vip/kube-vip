package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"time"
)

const udpdata = "a3ViZS12aXAK=kube-vip"

func main() {
	address := flag.String("address", "127.0.0.1", "The address of the server")
	port := flag.Int("port", 10002, "the port of the server")
	interval := flag.Float64("interval", 1000, "Interval in milliseconds")
	flag.Parse()
	var errorTime time.Time
	var errorOccurred bool
	for {
		p := make([]byte, 2048)
		conn, err := net.Dial("udp", fmt.Sprintf("%s:%d", *address, *port))
		if err != nil {
			if !errorOccurred {
				errorTime = time.Now()
				errorOccurred = true
			}
			continue
		}

		err = conn.SetDeadline(time.Now().Add(time.Duration(*interval) * time.Millisecond))
		if err != nil {
			//fmt.Printf("Connectivity error [%v]", err)
			if !errorOccurred {
				errorTime = time.Now()
				errorOccurred = true
			}
			if err = conn.Close(); err != nil {
				fmt.Printf("Error closing connection [%v]", err)
			}
			continue
		}

		_, err = fmt.Fprint(conn, udpdata)
		if err != nil {
			fmt.Printf("Error writing data [%v]", err)
		}

		_, err = bufio.NewReader(conn).Read(p)
		if err != nil {
			//fmt.Printf("read error %v\n", err)
			if !errorOccurred {
				errorTime = time.Now()
				errorOccurred = true
			}
			if err = conn.Close(); err != nil {
				fmt.Printf("Error closing connection [%v]", err)
			}
			continue
		}
		time.Sleep(time.Duration(*interval) * time.Millisecond)
		if errorOccurred {
			finishTime := time.Since(errorTime)
			//fmt.Printf("connectivity reconciled in %dms\n", finishTime.Milliseconds())
			//t :=time.Now().Format("15:04:05.000000")
			fmt.Printf("%s %d\n", time.Now().Format("15:04:05.000000"), finishTime.Milliseconds())

			errorOccurred = false
		}
	}
}
