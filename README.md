# kube-vip

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

`kibe-vip` provides both a floating or virtual IP address for your kubernetes cluster as well as load-balancing the incoming traffic to various control-plane replicas. At the current time to replicate this functionality a minimum of two pieces of tooling would be required:

**VIP**:
- [Keepalived](https://www.keepalived.org/)
- [Ucarp](https://ucarp.wordpress.com/)
- Hardware Load-balancer (functionality differs per vendor)


**LoadBalancing**:
- [HAProxy](http://www.haproxy.org/)
- [Nginx](http://nginx.com)
- Hardware Load-balancer(functionality differs per vendor)

All of these would require a separate level of configuration and in some infrastructures multiple teams in order to implement. Also when considering the software components, they may require packaging into containers or if they’re pre-packaged then security and transparency may be an issue. Finally, in edge environments we may have limited room for hardware (no HW load-balancer) or packages solutions in the correct architectures might not exist (e.g. ARM). Luckily with `kibe-vip` being written in GO, it’s small(ish) and easy to build for multiple architectures, with the added security benefit of being the only thing needed in the container.

## Architecture

### Cluster

To achieve HA, `kube-vip` requires multiple nodes and will use RAFT concensus to elect a leader amongst them. In the event that the leader fails for any reason, then a new election will take place and a new leader will be elected as part of the cluster. 

### Virtual IP

The leader within the cluster will assume the **vip** and will have it bound to the selected interace that is declared within the configuration. When the leader changes it will evacuate the **vip** first or in failure scenarios the **vip** will be directly assumed by the next elected leader.

When the **vip** moves from one host to another any host that has been using the **vip** will retain the previous `vip <-> MAC address` mapping until the ARP (Address resolution protocol) expires the old entry (typically 30 seconds) and retrieves a new `vip <-> MAC` mapping. This can be improved using Gratuitous ARP broadcasts (when enabled), this is detailed below.

### ARP

(Optional) The `kube-vip` can be configured to broadcast a [gratuitous arp](https://wiki.wireshark.org/Gratuitous_ARP) that will typically immediately notify all local hosts that the `vip <-> MAC` has changed.

### Load Balancing (Out of Cluster)

Within the configuration of `kube-vip` multiple load-balancers can be created, below is the example load-balancer for a Kubernetes Control-plane:

```
loadBalancers:
- name: Kubernetes Control Plane
  type: tcp
  port: 6443
  bindToVip: true
  backends:
  - port: 6444
    address: 192.168.0.70
  - port: 6444
    address: 192.168.0.71
  - port: 6444
    address: 192.168.0.72
```

The above load balancer will create an instance that listens on port `6443` and will forward traffic to the array of backend addresses. If the load-balancer type is `tcp` then the backends will be IP addresses, however if the backend is set to `http` then the backends should be URLs:

```
  type: http
  port: 6443
  bindToVip: true
  backends:
  - port: 6444
    address: https://192.168.0.70
```

Additionally the load-balancing within `kibe-vip` has two modes of operation:

`bindToVip: false` - will result in every node in the cluster binding all load-balancer port(s) to all interfaces on the host itself

`bindToVip: true` - The load-balancer will only **bind** to the VIP address.

## Usage

For In Cluster / Kubernetes `type:LoadBalancer` deployments follow the instructions [here](https://plndr.io/kube-vip/)

For providing HA and load-balancing for Kubernetes the documentation available here -> [kubernetes-control-plane.md](kubernetes-control-plane.md)

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


## Failover

A new leader is elected typically within a second of the previous leader failing, external hosts will see the changes differently based upon configuration.

### Without Gratuitous ARP

The failover will take however long their ARP caches are configured for, on most Linux systems the `vip <-> MAC` will be updated within 30 seconds.

### With Gratuitous ARP

With this enabled then the changes will propogate in a few seconds, as shown during a failed leader (`192.168.0.70` was restarted):

```
64 bytes from 192.168.0.75: icmp_seq=146 ttl=64 time=0.258 ms
64 bytes from 192.168.0.75: icmp_seq=147 ttl=64 time=0.240 ms
92 bytes from 192.168.0.70: Redirect Host(New addr: 192.168.0.75)
Vr HL TOS  Len   ID Flg  off TTL Pro  cks      Src      Dst
 4  5  00 0054 bc98   0 0000  3f  01 3d16 192.168.0.95  192.168.0.75 

Request timeout for icmp_seq 148
92 bytes from 192.168.0.70: Redirect Host(New addr: 192.168.0.75)
Vr HL TOS  Len   ID Flg  off TTL Pro  cks      Src      Dst
 4  5  00 0054 75ff   0 0000  3f  01 83af 192.168.0.95  192.168.0.75 

Request timeout for icmp_seq 149
92 bytes from 192.168.0.70: Redirect Host(New addr: 192.168.0.75)
Vr HL TOS  Len   ID Flg  off TTL Pro  cks      Src      Dst
 4  5  00 0054 2890   0 0000  3f  01 d11e 192.168.0.95  192.168.0.75 

Request timeout for icmp_seq 150
64 bytes from 192.168.0.75: icmp_seq=151 ttl=64 time=0.245 ms

```

## Troubleshooting

Enable debug logging by editing the `kube-vip.yaml` manifest and changing the `command:`:

```
  - command:
    - /kube-vip
    - start
    - -c
    - /vip.yaml
    - -l
    - "5"
 ```
