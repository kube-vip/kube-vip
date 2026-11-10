package kubevip

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"k8s.io/client-go/tools/leaderelection"
)

const (
	// nftables object names are limited to 255 bytes. Reserve space for the
	// prefix and address-family suffix added to the instance name.
	nftablesNameMaxLength     = 255
	egressNftablesTablePrefix = "kube_vip_"
	egressNftablesTableSuffix = "_v4"
	instanceNameMaxLength     = nftablesNameMaxLength - len(egressNftablesTablePrefix) - len(egressNftablesTableSuffix)
)

// Validate runs configuration checks that are independent of host state.
// This should be called after all config sources (flags, file, env vars) are merged.
func (c *Config) Validate() error {
	if err := validateHealthCheckAddress(c.ControlPlaneHealthCheck.Address); err != nil {
		return err
	}
	if err := validateInstanceName(c.InstanceName); err != nil {
		return err
	}
	if err := validateRoutingProtocol(c.RoutingProtocol); err != nil {
		return err
	}
	if err := validatePprof(c.EnablePprof, c.PprofHTTPServer); err != nil {
		return err
	}
	if c.usesKubernetesLeaderElection() {
		if err := validateLeaderElectionTimings(c.LeaseDuration, c.RenewDeadline, c.RetryPeriod); err != nil {
			return err
		}
	}

	return nil
}

// usesKubernetesLeaderElection reports whether kube-vip may run a Kubernetes
// Lease election with this configuration. Services are included even without
// leader or Service election enabled, as ARP and WireGuard modes always elect a
// Services leader and per-Service elections can be requested on demand.
func (c *Config) usesKubernetesLeaderElection() bool {
	if c.LeaderElectionType != "" && c.LeaderElectionType != "kubernetes" {
		return false
	}
	return c.EnableLeaderElection || c.EnableServicesElection || c.EnableServices
}

// validateLeaderElectionTimings rejects the lease timings (in seconds) that the
// client-go leader elector refuses. It panics on them once an election starts,
// so check them up front and report which setting has to change.
func validateLeaderElectionTimings(leaseDuration, renewDeadline, retryPeriod int) error {
	// client-go compares durations, so compare the same way to agree on the edge.
	jitteredRetryPeriod := time.Duration(leaderelection.JitterFactor * float64(time.Duration(retryPeriod)*time.Second))
	switch {
	case leaseDuration < 1:
		return fmt.Errorf("vip_leaseduration is %d, must be at least 1 second", leaseDuration)
	case renewDeadline < 1:
		return fmt.Errorf("vip_renewdeadline is %d, must be at least 1 second", renewDeadline)
	case retryPeriod < 1:
		return fmt.Errorf("vip_retryperiod is %d, must be at least 1 second", retryPeriod)
	case leaseDuration <= renewDeadline:
		return fmt.Errorf("vip_leaseduration (%d) must be greater than vip_renewdeadline (%d)", leaseDuration, renewDeadline)
	case time.Duration(renewDeadline)*time.Second <= jitteredRetryPeriod:
		return fmt.Errorf("vip_renewdeadline (%d) must be greater than vip_retryperiod (%d) * %g",
			renewDeadline, retryPeriod, leaderelection.JitterFactor)
	}
	return nil
}

// validatePprof rejects profiling being enabled with nowhere to serve it.
func validatePprof(enabled bool, addr string) error {
	if enabled && addr == "" {
		return fmt.Errorf("pprof is enabled but no pprof server address is set")
	}
	return nil
}

// validateRoutingProtocol rejects values the kernel cannot represent: netlink
// carries the address and route protocol in a single byte, so a larger value is
// silently truncated on the wire and never matches on readback.
func validateRoutingProtocol(protocol int) error {
	if protocol < 0 || protocol > math.MaxUint8 {
		return fmt.Errorf("routingProtocol %d is out of range, must be between 0 and %d", protocol, math.MaxUint8)
	}
	return nil
}

func validateInstanceName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > instanceNameMaxLength {
		return fmt.Errorf("instance_name is %d bytes, must not exceed %d bytes so the %q prefix and %q or %q suffix fit within the nftables %d-byte name limit",
			len(name), instanceNameMaxLength, egressNftablesTablePrefix, "_v4", "_v6", nftablesNameMaxLength)
	}

	for position, char := range name {
		if isValidNftablesNameCharacter(char) {
			continue
		}
		return fmt.Errorf("instance_name %q contains invalid character %q at byte %d; only ASCII letters, digits, '.', '-' and '_' are allowed",
			name, char, position)
	}
	return nil
}

func isValidNftablesNameCharacter(char rune) bool {
	return char >= 'a' && char <= 'z' ||
		char >= 'A' && char <= 'Z' ||
		char >= '0' && char <= '9' ||
		char == '_' || char == '-' || char == '.'
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
