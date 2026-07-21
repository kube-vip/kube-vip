package kubevip

import (
	"fmt"
	"net/url"
	"strings"
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
