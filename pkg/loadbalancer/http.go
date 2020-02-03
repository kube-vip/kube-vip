package loadbalancer

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	log "github.com/sirupsen/logrus"
	"github.com/thebsdbox/kube-vip/pkg/cluster"
)

//Start - begins the HTTP load balancer
func StartHTTP() error {
	log.Infoln("Starting any http load balancing services")
	for i := range config.Services {
		if config.Services[i].ServiceType == "http" {
			log.Debugf("Starting service [%s]", config.Services[i].Name)
			createHTTPHander(&config.Services[i])
		}
	}
	return nil
}

func createHTTPHander(s *cluster.Config) error {
	frontEnd := fmt.Sprintf("%s:%d", s.Address(), s.Port())

	handler := func(w http.ResponseWriter, req *http.Request) {
		// parse the url
		url, _ := url.Parse(s.ReturnEndpointURL().String())

		// create the reverse proxy
		proxy := httputil.NewSingleHostReverseProxy(url)

		// Update the headers to allow for SSL redirection
		req.URL.Host = url.Host
		req.URL.Scheme = url.Scheme
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Host = url.Host

		//Print out the response (if debug logging)
		if log.GetLevel() == log.DebugLevel {
			fmt.Printf("Host:\t%s\n", req.Host)
			fmt.Printf("Request:\t%s\n", req.Method)
			fmt.Printf("URI:\t%s\n", req.RequestURI)

			for key, value := range req.Header {
				fmt.Println("Header:", key, "Value:", value)
			}
		}

		// Note that ServeHttp is non blocking and uses a go routine under the hood
		proxy.ServeHTTP(w, req)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	log.Infof("Starting server listening [%s]", frontEnd)
	http.ListenAndServe(frontEnd, mux)
	// Should never get here
	return nil
}
