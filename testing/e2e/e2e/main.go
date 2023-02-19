package main

// This is largely to test outbound (egress) connections
import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
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
		for {

			// Connect to e2e endpoint with a second timeout
			conn, err := net.DialTimeout("tcp", address+":12345", time.Second)
			if err != nil {
				log.Fatalf("Dial failed: %v", err.Error())
			}
			_, err = conn.Write([]byte("The Grid, a digital frontier"))
			if err != nil {
				log.Fatalf("Write data failed: %v ", err.Error())
			}

			// buffer to get data
			received := make([]byte, 1024)
			_, err = conn.Read(received)
			if err != nil {
				log.Fatalf("Read data failed:", err.Error())
			}

			println("Received message: %s", string(received))

			conn.Close()
			// Wait for a second and connect again
			time.Sleep(time.Second)
		}
	default:
		log.Fatalf("Unknown mode [%s]", mode)
	}

}
