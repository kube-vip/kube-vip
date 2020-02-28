package loadbalancer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/plunder-app/kube-vip/pkg/kubevip"
	log "github.com/sirupsen/logrus"
)

func (lb *LBInstance) startHTTP(bindAddress string) error {
	log.Infof("Starting TCP Load Balancer for service [%s]", lb.instance.Name)

	// Validate the back end URLS
	err := kubevip.ValidateBackEndURLS(&lb.instance.Backends)
	if err != nil {
		return err
	}

	frontEnd := fmt.Sprintf("%s:%d", bindAddress, lb.instance.Port)

	handler := func(w http.ResponseWriter, req *http.Request) {
		// parse the url
		url, _ := url.Parse(lb.instance.ReturnEndpointURL().String())

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

	server := &http.Server{Addr: frontEnd, Handler: mux}

	go func() error {
		if err := server.ListenAndServe(); err != nil {
			return err
		}
		return nil
	}()

	// If the stop channel is closed then the server will be gracefully shut down
	<-lb.stop
	log.Infof("Stopping the load balancer [%s] bound to [%s] with 5sec timeout", lb.instance.Name, frontEnd)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		return err
	}
	close(lb.stopped)
	return nil
}

//StartHTTP - begins the HTTP load balancer
func StartHTTP(lb *kubevip.LoadBalancer, address string) error {
	log.Infof("Starting TCP Load Balancer for service [%s]", lb.Name)

	// Validate the back end URLS
	err := kubevip.ValidateBackEndURLS(&lb.Backends)
	if err != nil {
		return err
	}

	frontEnd := fmt.Sprintf("%s:%d", address, lb.Port)

	handler := func(w http.ResponseWriter, req *http.Request) {
		// parse the url
		url, _ := url.Parse(lb.ReturnEndpointURL().String())

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
