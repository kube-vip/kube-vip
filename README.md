# kube-vip


**NOTE** All documentation of both usage and architecture are now available at [https://kube-vip.io](https://kube-vip.io)


## Overview
Kubernetes Virtual IP and Load-Balancer for both control pane and Kubernetes services

The idea behind `kube-vip` is a small self-contained Highly-Available option for all environments, especially:

- Bare-Metal
- Edge (arm / Raspberry PI)
- Pretty much anywhere else :)

![](/overview.png)

The `kube-vip` application builds a multi-node or multi-pod cluster to provide High-Availability. When a leader is elected, this node will inherit the Virtual IP and become the leader of the load-balancing within the cluster. 

When running **out of cluster** it will use [raft](https://en.wikipedia.org/wiki/Raft_(computer_science)) clustering technology 

When running **in cluster** it will use [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection)

## Why?

The purpose of `kube-vip` is to simplify the building of HA Kubernetes clusters, which at this time can involve a few components and configurations that all need to be managed. This was blogged about in detail by [thebsdbox](https://twitter.com/thebsdbox/) here -> [https://thebsdbox.co.uk/2020/01/02/Designing-Building-HA-bare-metal-Kubernetes-cluster/#Networking-load-balancing](https://thebsdbox.co.uk/2020/01/02/Designing-Building-HA-bare-metal-Kubernetes-cluster/#Networking-load-balancing).

### Alternative HA Options

`kube-vip` provides both a floating or virtual IP address for your kubernetes cluster as well as load-balancing the incoming traffic to various control-plane replicas. At the current time to replicate this functionality a minimum of two pieces of tooling would be required:

**VIP**:
- [Keepalived](https://www.keepalived.org/)
- [Ucarp](https://ucarp.wordpress.com/)
- Hardware Load-balancer (functionality differs per vendor)


**LoadBalancing**:
- [HAProxy](http://www.haproxy.org/)
- [Nginx](http://nginx.com)
- Hardware Load-balancer(functionality differs per vendor)

All of these would require a separate level of configuration and in some infrastructures multiple teams in order to implement. Also when considering the software components, they may require packaging into containers or if they’re pre-packaged then security and transparency may be an issue. Finally, in edge environments we may have limited room for hardware (no HW load-balancer) or packages solutions in the correct architectures might not exist (e.g. ARM). Luckily with `kube-vip` being written in GO, it’s small(ish) and easy to build for multiple architectures, with the added security benefit of being the only thing needed in the container.


## Standalone Usage

The usage of `kube-vip` can either be directly by taking the binary / building yourself (`make build`), or alternatively through a pre-built docker container which can be found in the plunder Docker Hub repository [https://hub.docker.com/r/plndr/kube-vip](https://hub.docker.com/r/plndr/kube-vip). For further 

### Configuration

To generate the basic `yaml` configuration:

```
kube-vip sample config > config.yaml
```

Modify the `localPeer` section to match this particular instance (local IP address/port etc..) and ensure that the `remotePeers` section is correct for the current instance and all other instances in the cluster. Also ensure that the `interface` is the correct interface that the `vip` will bind to.


## Starting a simple cluster

To start `kube-vip` ensure the configuration for the `localPeers` and `remotePeers` is correct for each instance and the cluster as a whole and start:

```
kube-vip start -c /config.yaml
INFO[0000] Reading configuration from [config.yaml]       
INFO[0000] 2020-02-01T15:41:04.287Z [INFO]  raft: initial configuration: index=1 servers="[{Suffrage:Voter ID:server1 Address:192.168.0.70:10000} {Suffrage:Voter ID:server2 Address:192.168.0.71:10000} {Suffrage:Voter ID:server3 Address:192.168.0.72:10000}]" 
INFO[0000] 2020-02-01T15:41:04.287Z [INFO]  raft: entering follower state: follower="Node at 192.168.0.70:10000 [Follower]" leader= 
INFO[0000] Started                                      
INFO[0000] The Node [] is leading                       
INFO[0001] The Node [] is leading                       
INFO[0001] 2020-02-01T15:41:05.522Z [WARN]  raft: heartbeat timeout reached, starting election: last-leader= 
INFO[0001] 2020-02-01T15:41:05.522Z [INFO]  raft: entering candidate state: node="Node at 192.168.0.70:10000 [Candidate]" term=2 
INFO[0001] 2020-02-01T15:41:05.522Z [DEBUG] raft: votes: needed=2 
INFO[0001] 2020-02-01T15:41:05.522Z [DEBUG] raft: vote granted: from=server1 term=2 tally=1 
INFO[0001] 2020-02-01T15:41:05.523Z [DEBUG] raft: newer term discovered, fallback to follower 
INFO[0001] 2020-02-01T15:41:05.523Z [INFO]  raft: entering follower state: follower="Node at 192.168.0.70:10000 [Follower]" leader= 
INFO[0001] 2020-02-01T15:41:05.838Z [WARN]  raft: failed to get previous log: previous-index=2 last-index=1 error="log not found" 
INFO[0002] The Node [192.168.0.72:10000] is leading    
```

After a few seconds with additional nodes started a leader election will take place and the leader will assume the **vip**.
