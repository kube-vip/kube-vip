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
	var errorOccured bool
	for {
		p := make([]byte, 2048)
		conn, err := net.Dial("udp", fmt.Sprintf("%s:%d", *address, *port))
		conn.SetDeadline(time.Now().Add(time.Duration(*interval) * time.Millisecond))
		if err != nil {
			//fmt.Printf("Connectivity error [%v]", err)
			if !errorOccured {
				errorTime = time.Now()
				errorOccured = true
			}
			conn.Close()
			continue
		}

		fmt.Fprintf(conn, udpdata)

		_, err = bufio.NewReader(conn).Read(p)
		if err != nil {
			//fmt.Printf("read error %v\n", err)
			if !errorOccured {
				errorTime = time.Now()
				errorOccured = true
			}
			conn.Close()
			continue
		}
		time.Sleep(time.Duration(*interval) * time.Millisecond)
		if errorOccured {
			finishTime := time.Since(errorTime)
			//fmt.Printf("connectivity reconciled in %dms\n", finishTime.Milliseconds())
			//t :=time.Now().Format("15:04:05.000000")
			fmt.Printf("%s %d\n", time.Now().Format("15:04:05.000000"), finishTime.Milliseconds())

			errorOccured = false
		}
	}
}
