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
		log.Fatal("The environment variable E2ESERVER, was not set")
	}

	switch mode {
	case strings.ToUpper("SERVER"):
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "Hello!")
		})

		log.Info("Starting server at port 80")
		if err := http.ListenAndServe(":80", nil); err != nil {
			log.Fatal(err)
		}
	case strings.ToUpper("CLIENT"):
		address, exists := os.LookupEnv("E2EADDRESS")
		if !exists {
			log.Fatal("The environment variable E2EADDRESS, was not set")
		}
		ip := net.ParseIP(address)
		network := "tcp"
		port := ":12345"
		if ip.To4() == nil {
			network = "tcp6"
			address = fmt.Sprintf("[%s]", address)
			port = ":12346" // use a different port for IPv6 incase the IPv4 port is left connected
			log.Infoln("Connecting with IPv6")
		}
		for {
			// Connect to e2e endpoint with a second timeout
			conn, err := net.DialTimeout(network, address+port, time.Second)
			if err != nil {
				log.Errorf("Dial failed: %v", err.Error())
				// Wait for a second and connect again
				time.Sleep(time.Second)
				continue
			}
			_, err = conn.Write([]byte("The Grid, a digital frontier"))
			if err != nil {
				log.Errorf("Write data failed: %v ", err.Error())
				// Wait for a second and connect again
				time.Sleep(time.Second)
				continue
			}

			// buffer to get data
			received := make([]byte, 1024)
			_, err = conn.Read(received)
			if err != nil {
				log.Errorf("Read data failed: %v", err.Error())
				// Wait for a second and connect again
				time.Sleep(time.Second)
				continue
			}

			log.Infof("Received message: %s\n", string(received))

			conn.Close()
			// Wait for a second and connect again
			time.Sleep(time.Second)
		}
	default:
		log.Fatalf("Unknown mode [%s]", mode)
	}

}
