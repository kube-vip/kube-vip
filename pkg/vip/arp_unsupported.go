// +build !linux

package vip

import "fmt"

// ARPSendGratuitous is only supported on Linux, so return an error
func ARPSendGratuitous(address, ifaceName string) error {
	return fmt.Errorf("Unsupported on this OS")
}
