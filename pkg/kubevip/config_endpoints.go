package kubevip

import (
	"fmt"
	"net/url"
	"strconv"

	log "github.com/sirupsen/logrus"
)

func init() {
	// Start the index negative as it will be incrememnted of first approach
	endPointIndex = -1
}

// ValidateBackEndURLS will run through the endpoints and ensure that they're a valid URL
func ValidateBackEndURLS(endpoints *[]BackEnd) error {

	for i := range *endpoints {
		log.Debugf("Parsing [%s]", (*endpoints)[i].RawURL)
		u, err := url.Parse((*endpoints)[i].RawURL)
		if err != nil {
			return err
		}

		// No error is returned if the prefix/schema is missing
		// If the Host is empty then we were unable to parse correctly (could be prefix is missing)
		if u.Host == "" {
			return fmt.Errorf("Unable to parse [%s], ensure it's prefixed with http(s)://", (*endpoints)[i].RawURL)
		}
		(*endpoints)[i].Address = u.Hostname()
		// if a port is specified then update the internal endpoint stuct, if not rely on the schema
		if u.Port() != "" {
			portNum, err := strconv.Atoi(u.Port())
			if err != nil {
				return err
			}
			(*endpoints)[i].Port = portNum
		}
		(*endpoints)[i].ParsedURL = u
	}
	return nil
}

// ReturnEndpointAddr - returns an endpoint
func (lb LoadBalancer) ReturnEndpointAddr() (string, error) {
	if len(lb.Backends) == 0 {
		return "", fmt.Errorf("No Backends configured")
	}
	if endPointIndex < len(lb.Backends)-1 {
		endPointIndex++
	} else {
		// reset the index to the beginning
		endPointIndex = 0
	}
	// TODO - weighting, decision algorythmn
	return fmt.Sprintf("%s:%d", lb.Backends[endPointIndex].Address, lb.Backends[endPointIndex].Port), nil
}

// ReturnEndpointURL - returns an endpoint
func (lb LoadBalancer) ReturnEndpointURL() *url.URL {

	if endPointIndex != len(lb.Backends)-1 {
		endPointIndex++
	} else {
		// reset the index to the beginning
		endPointIndex = 0
	}
	// TODO - weighting, decision algorythmn
	return lb.Backends[endPointIndex].ParsedURL
}
