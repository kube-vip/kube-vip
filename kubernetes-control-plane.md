# Load Balancing a Kubernetes Cluster (Control-Plane)

This document covers all of the details for using `kube-vip` to build a HA Kubernetes cluster

`tl;dr version`
- Generate/modify first node `kube-vip` config/manifest
- `init` first node
- `join` remaining nodes
- Add remaining config/manifests

## Infrastructure architecture

The infrastructure for our example HA Kubernetes cluster is as follows:

| Node           | Address    |
|----------------|------------|
| VIP            | 10.0.0.75 |
| controlPlane01 | 10.0.0.70 |
| controlPlane02 | 10.0.0.71 |
| controlPlane03 | 10.0.0.72 |

All nodes are running Ubuntu 18.04, Docker CE and will use Kubernetes 1.17.0.

### Generate the `kube-vip` configuration

Make sure that the config directory exists: `sudo mkdir -p /etc/kube-vip/`, this directory can be any directory however the `hostPath` in the manifest will need modifying to point to the correct path.

```
sudo docker run -it --rm plndr/kube-vip:0.1 /kube-vip sample config | sudo tee /etc/kube-vip/config.yaml
```

### Modify the configuration

**Cluster Configuration**
Modify the `remotePeers` to point to the correct addresses of the other two nodes, ensure that their `id` is unique otherwise this will confuse the raft algorithm. The `localPeer` should be the configuration of the current node (`controlPlane01`), which is where this instance of the cluster will run. 

As this node will be the first node, it will need to elect itself leader as until this occurs the VIP won’t be activated!

`startAsLeader: true`

**VIP Config**
We will need to set our VIP address to `192.168.0.75` and to ensure all hosts are updated when the VIP moves we will enable ARP broadcasts `gratuitousARP: true`

**Load Balancer**
We will configure the load balancer to sit on the standard API-Server port `6443` and we will configure the backends to point to the API-servers that will be configured to run on port `6444`. Also for the Kubernetes Control Plane we will configure the load balancer to be of `type: tcp`.


**config.yaml**
`user@controlPlane01:/etc/kube-vip$ cat config.yaml`
...

``` 
remotePeers:
- id: server2
  address: 192.168.0.71
  port: 10000
- id: server3
  address: 192.168.0.72
  port: 10000
localPeer:
  id: server1
  address: 192.168.0.70
  port: 10000
vip: 192.168.0.75
gratuitousARP: true
singleNode: false
startAsLeader: true
interface: ens192
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

### First Node

To generate the basic Kubernetes static pod `yaml` configuration:

Make sure that the manifest directory exists: `sudo mkdir -p /etc/kubernetes/manifests/`

```
sudo docker run -it --rm plndr/kube-vip:0.1 /kube-vip sample manifest | sudo tee /etc/kubernetes/manifests/kube-vip.yaml
```

Ensure that `image: plndr/kube-vip:<x>` is modified to point to a specific version (`0.1` at the time of writing), refer to [docker hub](https://hub.docker.com/r/plndr/kube-vip/tags) for details. Also ensure that the `hostPath` points to the correct `kube-vip` configuration, if it isn’t the above path. 

The **vip** is set to `192.168.0.75` and this first node will elect itself as leader, and as part of the `kubeadm init` it will use the VIP in order to speak back to the initialising api-server.

`sudo kubeadm init --control-plane-endpoint “192.168.0.75:6444” --apiserver-bind-port 6443 --upload-certs --kubernetes-version “v1.17.0”`

Once this node is up and running we will be able to see the control-plane pods, including the `kube-vip` pod:

```
$ kubectl get pods -A
NAMESPACE     NAME                                     READY   STATUS    RESTARTS   AGE
<...>
kube-system   kube-vip-controlplane01                  1/1     Running   0          10m
```

### Remaining Nodes

We first will need to create the `kube-vip` configuration that resides in `/etc/kube-vip/config.yaml` or we can regenerate it from scratch using the above example. Ensure that the configuration is almost identical with the `localPeer` and `remotePeers` sections are updated for each node. Finally, ensure that the remaining nodes will behave as standard cluster nodes by setting `startAsLeader: false`.

At this point **DON’T** generate the manifests, this is due to some bizarre `kubeadm/kubelet` behaviour.

```
  kubeadm join 192.168.0.75:6444 --token <tkn> \
    --discovery-token-ca-cert-hash sha256:<hash> \
    --control-plane --certificate-key <key> 

```

**After** this node has been added to the cluster, we can add the manifest to also add this node as a `kibe-vip` member. (Adding the manifest afterwards doesn’t interfere with `kubeadm`). 

```
sudo docker run -it --rm plndr/kube-vip:0.1 /kube-vip sample manifest | sudo tee /etc/kubernetes/manifests/kube-vip.yaml
```

Once this node is added we will be able to see that the `kube-vip` pod is up and running as expected:

```
user@controlPlane01:~$ kubectl get pods -A | grep vip
kube-system   kube-vip-controlplane01                  1/1     Running             1          16m
kube-system   kube-vip-controlplane02                  1/1     Running             0          18m
kube-system   kube-vip-controlplane03                  1/1     Running             0          20m

```

If we look at the logs, we can see that the VIP is running on the second node and we’re waiting for our third node to join the cluster:

```
$ kubectl logs kube-vip-controlplane02  -n kube-system
time=“2020-02-09T15:33:09Z” level=info msg=“2020-02-09T15:33:09.285Z [ERROR] raft: failed to appendEntries to: peer=\”{Voter server3 192.168.0.72:10000}\” error=\”dial tcp 192.168.0.72:10000: connect: connection refused\””
time=“2020-02-09T15:33:09Z” level=info msg=“2020-02-09T15:33:09.650Z [ERROR] raft: failed to heartbeat to: peer=192.168.0.72:10000 error=\”dial tcp 192.168.0.72:10000: connect: connection refused\””
time=“2020-02-09T15:33:09Z” level=info msg=“2020-02-09T15:33:09.724Z [DEBUG] raft: failed to contact: server-id=server3 time=1h36m39.06317018s”
time=“2020-02-09T15:33:09Z” level=info msg=“The Node [192.168.0.71:10000] is leading”
```
