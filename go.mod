module github.com/kube-vip/kube-vip

go 1.19

require (
	github.com/cloudflare/ipvs v0.9.1
	github.com/davecgh/go-spew v1.1.1
	github.com/florianl/go-conntrack v0.4.0
	github.com/golang/protobuf v1.5.3
	github.com/insomniacslk/dhcp v0.0.0-20230731140434-0f9eb93a696c
	github.com/jpillora/backoff v1.0.0
	github.com/kamhlos/upnp v0.0.0-20210324072331-5661950dff08
	github.com/mdlayher/ndp v1.0.1
	github.com/onsi/ginkgo/v2 v2.11.0
	github.com/onsi/gomega v1.27.10
	github.com/osrg/gobgp/v3 v3.17.0
	github.com/packethost/packngo v0.30.0
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.16.0
	github.com/sirupsen/logrus v1.9.3
	github.com/spf13/cobra v1.7.0
	github.com/stretchr/testify v1.8.4
	github.com/vishvananda/netlink v1.2.1-beta.2
	golang.org/x/sys v0.10.0
	golang.zx2c4.com/wireguard/wgctrl v0.0.0-20230429144221-925a1e7659e6
	k8s.io/api v0.27.4
	k8s.io/apimachinery v0.27.4
	k8s.io/client-go v0.27.4
	k8s.io/klog/v2 v2.100.1
	sigs.k8s.io/kind v0.20.0
	sigs.k8s.io/yaml v1.3.0
)

require (
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/imdario/mergo v0.3.12 // indirect
	github.com/mdlayher/packet v1.1.2 // indirect
	github.com/pelletier/go-toml v1.9.5 // indirect
	github.com/pierrec/lz4/v4 v4.1.18 // indirect
	github.com/u-root/uio v0.0.0-20230305220412-3e8cd9d6bf63 // indirect
	golang.org/x/sync v0.3.0 // indirect
)
