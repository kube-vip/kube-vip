module github.com/plunder-app/kube-vip

go 1.14

require (
	github.com/armon/go-metrics v0.3.4 // indirect
	github.com/davecgh/go-spew v1.1.1
	github.com/dgryski/go-farm v0.0.0-20200201041132-a6ae2369ad13 // indirect
	github.com/digineo/go-dhclient v1.0.2
	github.com/eapache/queue v1.1.0 // indirect
	github.com/fatih/color v1.9.0 // indirect
	github.com/ghodss/yaml v1.0.0
	github.com/go-logr/logr v0.2.1 // indirect
	github.com/golang/protobuf v1.4.2
	github.com/google/go-cmp v0.5.2 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/google/gopacket v1.1.18
	github.com/google/uuid v1.1.2 // indirect
	github.com/googleapis/gnostic v0.5.1 // indirect
	github.com/hashicorp/go-hclog v0.14.1 // indirect
	github.com/hashicorp/go-immutable-radix v1.2.0 // indirect
	github.com/hashicorp/go-msgpack v1.1.5 // indirect
	github.com/hashicorp/go-retryablehttp v0.6.7 // indirect
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/hashicorp/raft v1.1.2
	github.com/imdario/mergo v0.3.11 // indirect
	github.com/k-sone/critbitgo v1.4.0 // indirect
	github.com/kamhlos/upnp v0.0.0-20171112074648-2713e75d9aef
	github.com/magiconair/properties v1.8.3 // indirect
	github.com/mattn/go-colorable v0.1.7 // indirect
	github.com/mdlayher/raw v0.0.0-20191009151244-50f2db8cc065 // indirect
	github.com/mitchellh/mapstructure v1.3.3 // indirect
	github.com/osrg/gobgp v2.0.0+incompatible
	github.com/packethost/packngo v0.5.1
	github.com/pelletier/go-toml v1.8.1 // indirect
	github.com/pkg/errors v0.9.1
	github.com/sirupsen/logrus v1.6.0
	github.com/spf13/afero v1.4.0 // indirect
	github.com/spf13/cast v1.3.1 // indirect
	github.com/spf13/cobra v1.0.0
	github.com/spf13/jwalterweatherman v1.1.0 // indirect
	github.com/spf13/viper v1.7.1 // indirect
	github.com/vishvananda/netlink v1.1.0
	github.com/vishvananda/netns v0.0.0-20200728191858-db3c7e526aae // indirect
	golang.org/x/crypto v0.0.0-20200820211705-5c72a883971a // indirect
	golang.org/x/net v0.0.0-20201021035429-f5854403a974
	golang.org/x/oauth2 v0.0.0-20200902213428-5d25da1a8d43 // indirect
	golang.org/x/sys v0.0.0-20200930185726-fdedc70b468f
	golang.org/x/time v0.0.0-20200630173020-3af7569d3a1e // indirect
	google.golang.org/genproto v0.0.0-20200917134801-bb4cff56e0d0 // indirect
	google.golang.org/grpc v1.32.0 // indirect
	gopkg.in/ini.v1 v1.61.0 // indirect
	k8s.io/api v0.19.2
	k8s.io/apimachinery v0.19.2
	k8s.io/client-go v0.19.0
	k8s.io/klog v1.0.0
	k8s.io/klog/v2 v2.3.0 // indirect
	k8s.io/utils v0.0.0-20200912215256-4140de9c8800 // indirect
)

replace (
	github.com/osrg/gobgp v2.0.0+incompatible => github.com/osrg/gobgp v0.0.0-20191101114856-a42a1a5f6bf0

	// CI fixes
	github.com/plunder-app/kube-vip/pkg/bgp => ./github.com/plunder-app/kube-vip/pkg/bgp
	github.com/plunder-app/kube-vip/pkg/cluster => ./github.com/plunder-app/kube-vip/pkg/cluster
	github.com/plunder-app/kube-vip/pkg/detector => ./github.com/plunder-app/kube-vip/pkg/detector
	github.com/plunder-app/kube-vip/pkg/kubevip => ./github.com/plunder-app/kube-vip/pkg/kubevip
	github.com/plunder-app/kube-vip/pkg/leaderElection => ./github.com/plunder-app/kube-vip/pkg/leaderElection
	github.com/plunder-app/kube-vip/pkg/loadbalancer => ./github.com/plunder-app/kube-vip/pkg/loadbalancer
	github.com/plunder-app/kube-vip/pkg/manager => ./github.com/plunder-app/kube-vip/pkg/manager
	github.com/plunder-app/kube-vip/pkg/packet => ./github.com/plunder-app/kube-vip/pkg/packet
)
