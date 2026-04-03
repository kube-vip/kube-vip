package kubevip

import (
	"fmt"
	"net/url"
	"strings"
)

// Validate runs configuration checks that are independent of host state.
// This should be called after all config sources (flags, file, env vars) are merged.
func (c *Config) Validate() error {
	if err := validateHealthCheckAddress(c.ControlPlaneHealthCheck.Address); err != nil {
		return err
	}

	return nil
}

func validateHealthCheckAddress(address string) error {
	if address == "" {
		return nil
	}

	parsedURL, err := url.ParseRequestURI(address)
	if err != nil {
		return fmt.Errorf("control_plane_health_check_address %q is not a valid URL: %w", address, err)
	}

	scheme := strings.ToLower(parsedURL.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("control_plane_health_check_address %q has unsupported scheme %q, expected http or https", address, parsedURL.Scheme)
	}

	return nil
}
