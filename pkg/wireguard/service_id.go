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
// legacy port-only identifier that must be removed during migration.
func ServicePortIDs(namespace, name string, port v1.ServicePort) (string, string) {
	rawServiceID := fmt.Sprintf("%s_%s", namespace, name)
	serviceID := utils.SanitizeServiceID(rawServiceID)
	legacyID := fmt.Sprintf("%s_p%d", serviceID, port.Port)
	protocol := port.Protocol
	if protocol == "" {
		protocol = v1.ProtocolTCP
	}
	suffix := fmt.Sprintf("_p%d_%s", port.Port, strings.ToLower(string(protocol)))
	if maxBase := maxServicePortIDLength - len(suffix); len(serviceID) > maxBase {
		sum := sha256.Sum256([]byte(rawServiceID))
		hash := fmt.Sprintf("_%x", sum[:8])
		serviceID = serviceID[:maxBase-len(hash)] + hash
	}
	return serviceID + suffix, legacyID
}
