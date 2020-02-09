# kube-vip (WIP)
Kubernetes Control Plane Virtual IP and Load-Balancer

The idea behind `kube-vip` is a small self-contained Highly-Available option for all environments, especially:

- Bare-Metal
- Edge (arm / Raspberry PI)
- Pretty much anywhere else :)

*Use at your own risk, although it's pretty stable :-) *

![](/overview.png)

The `kube-vip` application builds a multi-node cluster using [raft](https://en.wikipedia.org/wiki/Raft_(computer_science)) clustering technology to provide High-Availability. When a leader is elected, this node will inherit the Virtual IP and become the leader of the load-balancing within the cluster. 

## Architecture

### Cluster

To achieve HA, `kube-vip` requires multiple nodes and will use RAFT concensus to elect a leader amongst them. In the event that the leader fails for any reason, then a new election will take place and a new leader will be elected as part of the cluster. 

### Virtual IP

The leader within the cluster will assume the **vip** and will have it bound to the selected interace that is declared within the configuration. When the leader changes it will evacuate the **vip** first or in failure scenarios the **vip** will be directly assumed by the next elected leader.

When the **vip** moves from one host to another any host that has been using the **vip** will retain the previous `vip <-> MAC address` mapping until the ARP (Address resolution protocol) expires the old entry (typically 30 seconds) and retrieves a new `vip <-> MAC` mapping. This can be improved using Gratuitous ARP broadcasts (when enabled), this is detailed below.

### ARP

(Optional) The `kube-vip` can be configured to broadcast a [gratuitous arp](https://wiki.wireshark.org/Gratuitous_ARP) that will typically immediately notify all local hosts that the `vip <-> MAC` has changed.

### Load Balancing

Each node in teh cluster can act as a load balancer, but also each VIP can be its own load balancer. This provides various architectural options to how your cluster is designed.

#### Per Node LB

*PROS*

- Simple and doesn't require a tear-up/tear-down approach

*CONS*

- Wasted LB instances that aren't being used
- Require a seperate port to the service being load balanced


#### VIP LB

*PROS*

- The VIP can use the same port as the underlying service without conflicting on the port

*CONS*

- Requires `kube-vip` to manage stopping and starting of LB services every election
- Slightly complex design

## Usage

### Configuration 

To generate the basic `yaml` configuration:

**Without Docker**
```
kube-vip sample config > config.yaml
```

**With Docker**
```
sudo docker run -it --rm plndr/kube-vip:0.1 /kube-vip sample config | sudo tee /etc/kube-vip/config.yaml
```

Modify the `localPeer` section to match this particular instance (local IP address/port etc..) and ensure that the `remotePeers` section is correct for the current instance and all other instances in the cluster. Also ensure that the `interface` is the correct interface that teh `vip` will bind to.


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

## Load Balancing a Kubernetes Cluster (Control-Plane)

### Generate the Static pod configuration

Make sure that the Manifests directory exists: `sudo mkdir /etc/kubernetes/manifests/`

To generate the basic Kubernetes static pod `yaml` configuration:

**Without Docker**
```
sudo kube-vip sample manifest | sudo tee /etc/kubernetes/manifests/kube-vip.yaml
```

**With Docker**
```
sudo docker run -it --rm plndr/kube-vip:0.1 /kube-vip sample manifest | sudo tee /etc/kubernetes/manifests/kube-vip.yaml
```

### Modify the configuration

Ensure that `image: plndr/kube-vip:latest` is modified to point to a specific version (`0.1` at the time of writing), refer to [docker hub](https://hub.docker.com/r/plndr/kube-vip/tags) for details. Also ensure that the `hostPath` points to the correct `kube-vip` configuration. For the Control-plane we will also be using the load balancer type `tcp` so modify the kubernetes load-balancer:

```
loadBalancers:
- name: Kubernetes Control Plane
  type: tcp
```

### First Node

As a raft election wont happen unless multiple nodes are running, we will need to start the first node in `singleNode: true`. This means that the load balancer will be up and running and that `kubeadm` will be able to use it as expected.

The **vip** is set to `192.168.0.77` and the cluster is in `singleNode` mode.

`sudo kubeadm init --control-plane-endpoint "192.168.0.77:6444" --apiserver-bind-port 6443 --upload-certs --kubernetes-version "v1.17.0"`

Once our `kubeadm` is completed and we have the first node initialised we can disable the `singleNode` configuration, we will need to restart the `kube-vip` container in the pod in order for it to pick up the new configuration changes. A `docker kill <kube-vip container id>` will mean that Kubernetes will recreate the pod correctly.  

### Remaining Nodes

Create the configuration as shown above, but ensure that teh `localPeer` and `remotePeers` sections are updated for each node and we can use the join command from the output of the first node. Also copy the `kube-vip` manifest from the first node to the remaining nodes into `/etc/kubernetes/manifests/`

**Note**, we will need to add an ignore-preflight-error as we've added our file into this directory

```
  kubeadm join 192.168.0.77:6444 --token <tkn> \
    --discovery-token-ca-cert-hash sha256:<hash> \
    --control-plane --certificate-key <key> \
    --ignore-preflight-errors=DirAvailable--etc-kubernetes-manifests
```

Once this node is added we will be able to see that the `kube-vip` pod is up and running as expected:

```
user@controlPlane01:~$ kubectl get pods -A | grep vip
kube-system   kube-vip-controlplane01                  1/1     Running             1          96m
kube-system   kube-vip-controlplane02                  1/1     Running             0          98m
```

If we look at the logs, we can see that the VIP is running on the second node and we're waiting for our third node to join the cluster:

```
$ kubectl logs kube-vip-controlplane02  -n kube-system
time="2020-02-09T15:33:09Z" level=info msg="2020-02-09T15:33:09.285Z [ERROR] raft: failed to appendEntries to: peer=\"{Voter server3 192.168.0.72:10000}\" error=\"dial tcp 192.168.0.72:10000: connect: connection refused\""
time="2020-02-09T15:33:09Z" level=info msg="2020-02-09T15:33:09.650Z [ERROR] raft: failed to heartbeat to: peer=192.168.0.72:10000 error=\"dial tcp 192.168.0.72:10000: connect: connection refused\""
time="2020-02-09T15:33:09Z" level=info msg="2020-02-09T15:33:09.724Z [DEBUG] raft: failed to contact: server-id=server3 time=1h36m39.06317018s"
time="2020-02-09T15:33:09Z" level=info msg="The Node [192.168.0.71:10000] is leading"
```

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
