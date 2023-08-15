package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

func main() {
	serverType := strings.ToLower(os.Getenv("serverType"))

	if serverType == "tcp" {
		// Start the TCP echo server
		l, err := net.Listen("tcp", ":10001")
		if err != nil {
			fmt.Println("ERROR", err)
			os.Exit(1)
		}
		fmt.Println("Started TCP server on port 10001")

		for {
			conn, err := l.Accept()
			if err != nil {
				fmt.Println("ERROR", err)
				continue
			}
			go echo(conn)

		}
	}

	if serverType == strings.ToLower("udp") {
		// Start the UDP echo server

		ServerAddr, err := net.ResolveUDPAddr("udp", ":10002")
		if err != nil {
			fmt.Println("Error: ", err)
			return
		}
		fmt.Println("Started UDP server on port 10002")

		/* Now listen at selected port */
		ServerConn, err := net.ListenUDP("udp", ServerAddr)
		if err != nil {
			fmt.Println("Error: ", err)
			return
		}
		defer ServerConn.Close()

		buf := make([]byte, 1024)

		for {
			n, addr, err := ServerConn.ReadFromUDP(buf)
			fmt.Printf("received: %s from: %s\n", string(buf[0:n]), addr)

			if err != nil {
				fmt.Println("error: ", err)
			}

			ServerConn.WriteTo(buf[0:n])
		}
	}
}

func echo(conn net.Conn) {
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes(byte('\n'))
		switch err {
		case nil:
			break
		case io.EOF:
		default:
			fmt.Println("ERROR", err)
		}
		conn.Write(line)
	}
}
