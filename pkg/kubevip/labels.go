package kubevip

import (
	"slices"
)

const (
	// ServiceProvided is the name of the label that will be added to the node
	ServiceProvided = "service-provided.kube-vip.io"

	// label used on nodes, which announce the LoadBalancer IP
	HasIP = "kube-vip.io/has-ip"
)

var kubevipLabelKeys = []string{
	ServiceProvided,
	HasIP,
}

func GetKeysForCleanup() []string {
	return slices.Clone(kubevipLabelKeys)
}
