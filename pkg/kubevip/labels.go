package kubevip

const (
	// label used on nodes, which announce the LoadBalancer IP
	HasIP         = "kube-vip.io/has-ip"
	HasIPJSONPath = `kube-vip.io~1has-ip`
)
