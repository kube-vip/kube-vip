package kubevip

const (
	LBClassName = "kube-vip.io/kube-vip-class"

	DefaultDHCPBackoffAttempts = 3

	// DefaultEgressVerificationInterval is the interval in seconds between SNAT rule checks.
	DefaultEgressVerificationInterval = 60
)
