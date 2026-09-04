package wireguard

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/kube-vip/kube-vip/pkg/utils"
	v1 "k8s.io/api/core/v1"
)

const maxServicePortIDLength = 50

// ServicePortIDs returns the protocol-qualified nftables identifier and the
// prior port-only identifier that must be removed during migration.
//
// Sanitisation maps '-' onto the '_' separator, so "a-b/c" and "a/b-c" would
// otherwise share a chain; a hash of the raw name keeps them distinct.
func ServicePortIDs(namespace, name string, port v1.ServicePort) (string, string) {
	rawServiceID := fmt.Sprintf("%s_%s", namespace, name)
	serviceID := utils.SanitizeServiceID(rawServiceID)
	legacyID := fmt.Sprintf("%s_p%d", serviceID, port.Port)
	protocol := port.Protocol
	if protocol == "" {
		protocol = v1.ProtocolTCP
	}
	suffix := fmt.Sprintf("_p%d_%s", port.Port, strings.ToLower(string(protocol)))
	maxBase := maxServicePortIDLength - len(suffix)
	if serviceID != rawServiceID || len(serviceID) > maxBase {
		sum := sha256.Sum256([]byte(rawServiceID))
		hash := fmt.Sprintf("_%x", sum[:8])
		maxPrefix := maxBase - len(hash)
		if len(serviceID) > maxPrefix {
			serviceID = serviceID[:maxPrefix]
		}
		serviceID += hash
	}
	return serviceID + suffix, legacyID
}
