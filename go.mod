module github.com/kube-vip/kube-vip

go 1.14

require (
	github.com/cloudflare/ipvs v0.8.0
	github.com/davecgh/go-spew v1.1.1
	github.com/ghodss/yaml v1.0.0
	github.com/golang/protobuf v1.5.2
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/imdario/mergo v0.3.12 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20211214070828-5297eed8f489
	github.com/jpillora/backoff v1.0.0
	github.com/kamhlos/upnp v0.0.0-20210324072331-5661950dff08
	github.com/mdlayher/ndp v0.0.0-20210831201139-f982b8766fb5
	github.com/mdlayher/raw v0.0.0-20211126142749-4eae47f3d54b // indirect
	github.com/onsi/ginkgo v1.14.2
	github.com/onsi/gomega v1.10.1
	github.com/osrg/gobgp v0.0.0-20210901090119-8ab84f8636ee
	github.com/packethost/packngo v0.20.0
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.11.0
	github.com/sirupsen/logrus v1.8.1
	github.com/spf13/cobra v1.3.0
	github.com/stretchr/testify v1.7.0
	github.com/vishvananda/netlink v1.1.1-0.20210330154013-f5de75959ad5
	github.com/vishvananda/netns v0.0.0-20211101163701-50045581ed74 // indirect
	golang.org/x/net v0.0.0-20211216030914-fe4d6282115f
	golang.org/x/sys v0.0.0-20211216021012-1d35b9e2eb4e
	golang.org/x/term v0.0.0-20210927222741-03fcf44c2211 // indirect
	k8s.io/api v0.22.5
	k8s.io/apimachinery v0.22.5
	k8s.io/client-go v0.22.5
	k8s.io/klog/v2 v2.40.1
	sigs.k8s.io/kind v0.11.1
)
