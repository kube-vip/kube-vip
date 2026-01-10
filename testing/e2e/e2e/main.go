package main

// This is largely to test outbound (egress) connections
import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	log "log/slog"
)

func main() {
	// Lookup environment variables
	mode, exists := os.LookupEnv("E2EMODE")
	if !exists {
		log.Error("The environment variable E2ESERVER, was not set")
		os.Exit(1)
	}

	switch mode {
	case strings.ToUpper("SERVER"):
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "Hello!")
		})

		log.Info("starting server at port 80")
		if err := http.ListenAndServe(":80", nil); err != nil {
			log.Error(err.Error())
			os.Exit(1)
		}
	case strings.ToUpper("CLIENT"):
		address, exists := os.LookupEnv("E2EADDRESS")
		if !exists {
			log.Error("the environment variable E2EADDRESS, was not set")
			os.Exit(1)
		}
		ip := net.ParseIP(address)
		network := "tcp"
		port := ":12345"
		if ip.To4() == nil {
			network = "tcp6"
			address = fmt.Sprintf("[%s]", address)
			port = ":12346" // use a different port for IPv6 incase the IPv4 port is left connected
			log.Info("connecting with IPv6")
		}
		for {
			// Connect to e2e endpoint with a second timeout
			conn, err := net.DialTimeout(network, address+port, time.Second)
			if err != nil {
				log.Error("dial failed", "err", err)
				// Wait for a second and connect again
				time.Sleep(time.Second)
				continue
			}
			_, err = conn.Write([]byte("The Grid, a digital frontier"))
			if err != nil {
				log.Error("write data  failed", "err", err)
				// Wait for a second and connect again
				time.Sleep(time.Second)
				continue
			}

			// buffer to get data
			received := make([]byte, 1024)
			_, err = conn.Read(received)
			if err != nil {
				log.Error("Read data failed", "err", err)
				// Wait for a second and connect again
				time.Sleep(time.Second)
				continue
			}

			log.Info("received", "message", string(received))

			conn.Close()
			// Wait for a second and connect again
			time.Sleep(time.Second)
		}
	default:
		log.Error("Unknown mode", "value", mode)
		os.Exit(1)
	}
}
