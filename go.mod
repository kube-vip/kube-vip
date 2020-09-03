module github.com/plunder-app/kube-vip

go 1.14

require (
	github.com/davecgh/go-spew v1.1.1
	github.com/dgryski/go-farm v0.0.0-20200201041132-a6ae2369ad13 // indirect
	github.com/digineo/go-dhclient v1.0.2
	github.com/eapache/queue v1.1.0 // indirect
	github.com/ghodss/yaml v1.0.0
	github.com/golang/protobuf v1.3.2
	github.com/hashicorp/raft v1.1.2
	github.com/imdario/mergo v0.3.8 // indirect
	github.com/osrg/gobgp v2.0.0+incompatible
	github.com/packethost/packngo v0.2.1-0.20200424110205-36917dbc292f
	github.com/pkg/errors v0.9.1
	github.com/sirupsen/logrus v1.4.2
	github.com/spf13/cobra v0.0.6
	github.com/vishvananda/netlink v1.1.0
	golang.org/x/net v0.0.0-20191004110552-13f9640d40b9
	k8s.io/api v0.18.0
	k8s.io/apimachinery v0.18.0
	k8s.io/client-go v0.18.0
	k8s.io/klog v1.0.0
	sigs.k8s.io/structured-merge-diff v0.0.0-20190525122527-15d366b2352e // indirect
)

replace github.com/osrg/gobgp v2.0.0+incompatible => github.com/osrg/gobgp v0.0.0-20191101114856-a42a1a5f6bf0
