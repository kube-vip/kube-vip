# Load Balancing a Kubernetes Cluster (Control-Plane)

**Note**: The most common deployment currently for HA Kubernetes clusters w/`kub-vip` involved `kubeadm`, however recently we've worked to bring a method of bringing `kube-vip` to other types of Kubernetes cluster. Typically this deployment method makes use of a daemonset that is usually brought up during the cluster instantiation.. So for those wanting to deploy [k3s](https://k3s.io), we now have installation steps available [here](https://kube-vip.io/control-plane/#k3s),

This document covers the newer (post `0.1.6`) method for using `kube-vip` to provide HA for a Kubernetes Cluster. The documentation for older releases can be found [here](./0.1.5/)

From version `0.1.6` we've moved `kube-vip` from raft to leaderElection within the Kubernetes cluster. After a lot of testing it became clear that the leaderElection gave quicker reconciliation when removing nodes etc.. during upgrades and failures.

For **more** configuration around LeaderElection click [here](https://kube-vip.io/control-plane/#leaderelection-configuration).

This document covers all of the details for using `kube-vip` to build a HA Kubernetes cluster

`tl;dr version`
- Generate/modify first node `kube-vip` config/manifest
- `init` first node
- `join` remaining nodes
- Add remaining config/manifests

Below are examples of the steps required:

```
# First Node
sudo docker run --network host --rm plndr/kube-vip:0.2.1 manifest pod \
--interface ens192 \
--vip 192.168.0.75 \
--arp \
--leaderElection | sudo tee /etc/kubernetes/manifests/vip.yaml

sudo kubeadm init --kubernetes-version 1.17.0 --control-plane-endpoint 192.168.0.75 --upload-certs

# Additional Node(s)

sudo kubeadm join 192.168.0.75:6443 --token w5atsr.blahblahblah --control-plane --certificate-key abc123

sudo docker run --network host --rm plndr/kube-vip:0.2.1 manifest pod \
--interface ens192 \
--vip 192.168.0.75 \
--arp \
--leaderElection | sudo tee /etc/kubernetes/manifests/vip.yaml
```


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

`kube-vip` no longer requires storing its configuration in a separate directory and will now store its configuration in the actual manifest that defines the static pods. 

```
sudo docker run --network host \
	--rm plndr/kube-vip:0.2.1 \
	manifest pod \
	--interface ens192 \
	--vip 192.168.0.75 \
	--arp \
	--leaderElection | sudo tee /etc/kubernetes/manifests/vip.yaml
```

The above command will "initialise" the manifest within the `/etc/kubernetes/manifests` directory, that will be started when we actually initialise our Kubernetes cluster with `kubeadm init`

### Modify the configuration

**Cluster Configuration**
To enable Kubernetes leader Election passing the `--leaderElection` flag will enable `kube-vip` to use the Kubernetes leaderElection functionality to work out which member is the leader.

**VIP Config**
We will need to set our VIP address to `192.168.0.75` with `--vip 192.168.0.75` and to ensure all hosts are updated when the VIP moves we will enable ARP broadcasts `--arp` (defaults to `true`)

**vip.yaml** Static-pod Manifest

`$ sudo cat /etc/kubernetes/manifests/vip.yaml`


``` 
apiVersion: v1
kind: Pod
metadata:
  creationTimestamp: null
  name: kube-vip
  namespace: kube-system
spec:
  containers:
  - args:
    - start
    env:
    - name: vip_arp
      value: "true"
    - name: vip_interface
      value: ens160
    - name: vip_leaderelection
      value: "true"
    - name: vip_leaseduration
      value: "5"
    - name: vip_renewdeadline
      value: "3"
    - name: vip_retryperiod
      value: "1"
    - name: vip_address
      value: 192.168.0.75
    image: plndr/kube-vip:0.2.1
    imagePullPolicy: Always
    name: kube-vip
    resources: {}
    securityContext:
      capabilities:
        add:
        - NET_ADMIN
        - SYS_TIME
  hostNetwork: true
status: {}
```

### First Node

To generate the basic Kubernetes static pod `yaml` configuration:

Make sure that the manifest directory exists: `sudo mkdir -p /etc/kubernetes/manifests/`

```
sudo docker run --network host \
	--rm plndr/kube-vip:0.2.1 \
	manifest pod \
	--interface ens192 \
	--vip 192.168.0.75 \
	--arp \
	--leaderElection | sudo tee /etc/kubernetes/manifests/vip.yaml
```

Ensure that `image: plndr/kube-vip:<x>` is modified to point to a specific version (`0.1.8` at the time of writing), refer to [docker hub](https://hub.docker.com/r/plndr/kube-vip/tags) for details. 

The **vip** is set to `192.168.0.75` and this first node will elect itself as leader, and as part of the `kubeadm init` it will use the VIP in order to speak back to the initialising api-server.

`sudo kubeadm init --control-plane-endpoint “192.168.0.75:6443” --upload-certs --kubernetes-version “v1.17.0”`

Once this node is up and running we will be able to see the control-plane pods, including the `kube-vip` pod:

```
$ kubectl get pods -A
NAMESPACE     NAME                                     READY   STATUS    RESTARTS   AGE
<...>
kube-system   kube-vip-controlplane01                  1/1     Running   0          10m
```

### Remaining Nodes


At this point **DON’T** generate the manifests, this is due to some bizarre `kubeadm/kubelet` behaviour.

```
  kubeadm join 192.168.0.75:6443 --token <tkn> \
    --discovery-token-ca-cert-hash sha256:<hash> \
    --control-plane --certificate-key <key> 

```

**After** this node has been added to the cluster, we can add the manifest to also add this node as a `kube-vip` member. (Adding the manifest afterwards doesn’t interfere with `kubeadm`). 

```
sudo docker run --network host \
	--rm plndr/kube-vip:0.2.1 \
	manifest pod \
	--interface ens192 \
	--vip 192.168.0.75 \
	--arp \
	--leaderElection | sudo tee /etc/kubernetes/manifests/vip.yaml

```

Once this node is added we will be able to see that the `kube-vip` pod is up and running as expected:

```
user@controlPlane01:~$ kubectl get pods -A | grep vip
kube-system   kube-vip-controlplane01                  1/1     Running             1          16m
kube-system   kube-vip-controlplane02                  1/1     Running             0          18m
kube-system   kube-vip-controlplane03                  1/1     Running             0          20m

```

## DNS Support

### Static DNS Support (added in 0.2.0)

A new flag `--address` is introduced to support using a DNS record as the control plane endpoint. `kube-vip` will do a dns lookup to retrieve the IP for the DNS record, and use that IP as the VIP. An `dnsUpdater` periodically checks and updates the system if IP changes for the DNS record.

### Dynamic DNS Support (added in 0.2.1)

`kube-vip` was also updated to support DHCP + [Dynamic DNS](https://en.wikipedia.org/wiki/Dynamic_DNS), for the use case where it's not able to reserve a static IP for the control plane endpoint.

A new flag `--ddns` is introduced. Once enabled, `kube-vip` expects the input `--address` will be a FQDN without binding to an IP. Then `kube-vip` will start a dhcp client to allocate an IP for the hostname of FQDN, and maintain the lease for it.

Once DHCP returns an IP for the FQDN, the same `dnsUpdater` runs to periodically checks and updates if IP got changed.

## BGP Support (added in 0.1.8)

In version `0.1.8`+ `kube-vip` was updated to support [BGP](https://en.wikipedia.org/wiki/Border_Gateway_Protocol) as a VIP failover mechanism. When a node is elected as a leader then it will update it's peers so that they are aware to route traffic to that node in order to access the VIP. 

The following new flags are used:

- `--bgp` This will enable BGP support within kube-vip
- `--localAS` The local AS number
- `--bgpRouterID` The local router address
- `--peerAS` The AS number for a BGP peer
- `--peerAddress` The address of a BGP peer

### BGP Packet support

If the `--bgp` flag is passed alone with the Packet flags `packet, packetKey and packetProject`, then the Packet API will be used in order to determine the BGP configuration for the nodes being used in the cluster. This automates a lot of the process and makes using BGP within Packet much simpler.

## Packet Support (added in 0.1.7)

Recently in version `0.1.7` of `kube-vip` we added the functionality to use a Packet Elastic IP as the virtual IP fronting the Kubernetes Control plane cluster. In order to first get out virtual IP we will need to use our Packet account and create a EIP (either public (eek) or private). We will only need a single address so a `/32` will suffice, once this is created as part of a Packet project we can now apply this address to the servers that live in the same project. 

In this example we've logged into the UI can created a new EIP of `147.75.1.2`, and we've deployed three small server instances with Ubuntu.

The following new flags are used:

- `--packet` which enables the use of the Packet API
- `--packetKey` which is our API key
- `--packetProject`which is the name of our Packet project where our servers and EIP are located.

*Also* the `--arp` flag should NOT be used as it wont work within the Packet network.

### Variables

```
export EIP=1.1.1.1
export PACKET_AUTH_TOKEN=XYZ
```

### First node

```
# Generate the manifest

sudo docker run --network host --rm plndr/kube-vip:0.2.1 manifest pod \
--arp=false \
--interface lo \
--vip $EIP \
--leaderElection \
--packet \
--packetKey $PACKET_AUTH_TOKEN \
--packetProject vipTest | sudo tee /etc/kubernetes/manifests/vip.yaml\

# Init Kubernetes

sudo kubeadm init --kubernetes-version 1.18.5 --control-plane-endpoint $EIP --upload-certs
```

### Other nodes

```
# Join
kubeadm join $EIP:6443 --token BLAH --control-plane --certificate-key BLAH --discovery-token-ca-cert-hash sha:blah

# Generate Manifest

sudo docker run --network host --rm plndr/kube-vip:0.2.1 manifest pod \
--arp=false \
--interface lo \
--vip $EIP \
--leaderElection \
--packet \
--packetKey $PACKET_AUTH_TOKEN \
--packetProject vipTest | sudo tee /etc/kubernetes/manifests/vip.yaml\
```

The Elastic IP failover takes some time (30+ seconds) to move from a failed host to a new leader, so in this release it is mainly for testing. 


## Upgrades

From above we have a 3 node cluster and the controlPlane01 is leader:

```
$ kubectl logs -n kube-system kube-vip-controlplane01 -f
time="2020-07-04T15:12:52Z" level=info msg="Beginning cluster membership, namespace [kube-system], lock name [plunder-lock], id [controlPlane01]"
I0704 15:12:52.290420       1 leaderelection.go:242] attempting to acquire leader lease  kube-system/plunder-lock...
I0704 15:12:56.373113       1 leaderelection.go:252] successfully acquired lease kube-system/plunder-lock
time="2020-07-04T15:12:56Z" level=info msg="This node is assuming leadership of the cluster"
time="2020-07-04T15:12:56Z" level=error msg="This node is leader and is adopting the virtual IP"
time="2020-07-04T15:12:56Z" level=info msg="Starting TCP Load Balancer for service [192.168.0.81:0]"
time="2020-07-04T15:12:56Z" level=info msg="Load Balancer [Kubeadm Load Balancer] started"
time="2020-07-04T15:12:56Z" level=info msg="Broadcasting ARP update for 192.168.0.81 (00:50:56:a5:69:a1) via ens192"
time="2020-07-04T15:12:56Z" level=info msg="Starting TCP Load Balancer for service [192.168.0.81:0]"
time="2020-07-04T15:12:56Z" level=info msg="Load Balancer [Kubeadm Load Balancer] started"
time="2020-07-04T15:12:56Z" level=info msg="Broadcasting ARP update for 192.168.0.81 (00:50:56:a5:69:a1) via ens192"
time="2020-07-04T15:12:56Z" level=info msg="new leader elected: controlPlane01"
```

We will kill this node and watch `kube-vip` logs from another node:

#### Pinging VIP

```
64 bytes from 192.168.0.81: icmp_seq=667 ttl=64 time=0.387 ms
Request timeout for icmp_seq 668
Request timeout for icmp_seq 669
Request timeout for icmp_seq 670
Request timeout for icmp_seq 671
Request timeout for icmp_seq 672
64 bytes from 192.168.0.81: icmp_seq=673 ttl=64 time=0.453 ms
```

#### Logs 
```
$ kubectl logs -n kube-system kube-vip-controlplane03 -f
time="2020-07-04T15:17:53Z" level=info msg="Beginning cluster membership, namespace [kube-system], lock name [plunder-lock], id [controlPlane03]"
I0704 15:17:53.484698       1 leaderelection.go:242] attempting to acquire leader lease  kube-system/plunder-lock...
time="2020-07-04T15:17:53Z" level=info msg="new leader elected: controlPlane01"
E0704 15:20:18.864141       1 leaderelection.go:331] error retrieving resource lock kube-system/plunder-lock: etcdserver: request timed out
time="2020-07-04T15:20:20Z" level=info msg="new leader elected: controlPlane02"
```

#### Adding `controlPlane04`

A kubeadm join will fail as the `controlPlane01` still exists as an endpoint, so we have two options (manual steps and configmap edit to remove all mention of this node, or we can bring this node up and `kubeadm reset` the node (which we will do)). 

```
$ kubectl get nodes
NAME             STATUS     ROLES    AGE   VERSION
controlplane01   NotReady   master   14m   v1.17.0
controlplane02   Ready      master   13m   v1.17.2
controlplane03   Ready      master   13m   v1.17.0
controlplane04   NotReady   master   9s    v1.17.0
```
 After this we can add this node into `kube-vip` with the same manifest created by `docker run`.

## LeaderElection configuration

The Kubernetes LeaderElection that is used to manage the election of a new leader now supports having it's settings managed through flags. 

 - `--leaseDuration` Length of time a Kubernetes leader lease can be held for
 - `--leaseRenewDuration` Length of time a Kubernetes leader can attempt to renew its lease
 - `--leaseRetry` Number of times the host will retry to hold a lease

For larger clusters the `--leaseDuration` and `--leaseRenewDuration` may need extending due to slower `etcd` performance. (Tested with 2000 nodes)

## k3s

This section details the steps required to deploye `k3s` in a Highly available manner, using kube-vip deployed within k3s as a daemonset on the control plane nodes. As of `k3s` v1 the persistent datastore is back to etcd, however this guide will also include the steps for using `mysql`.

### Example MySQL deployment (optional)

To quickly validate this we can use docker on a host to quickly spin up a mysql database to store the persistent Kubernetes data.

#### Create local directory for BD storage
`mkdir mysql`

#### Start Docker MySQL container
`sudo docker run --cap-add SYS_NICE -p 3306:3306 --name k3s-mysql -v /home/dan/mysql:/var/lib/mysql -e MYSQL_ROOT_PASSWORD=k3s-password -d mysql:8`

### Create `kube-vip` manifest

The `kube-vip` manifest contains all the configuration for starting up `kube-vip` within the `k3s` cluster, it runs as a daemonset with affinity/taints for the control-plane nodes. As `k3s` starts it will parse all manifests in the manifests folder and start the highly available VIP across all control plane nodes in the cluster.

#### Create the `k3` manifests directory

Create the manifests directory, this directory is used by `k3s` for all of it's other deployments once it's up and running.
`sudo mkdir -p /var/lib/rancher/k3s/server/manifests/`

#### Generate the manifest

Modify the `vipAddress` and `vipInterface` to match the floating IP address you'd like to use and the interface it should bind to.

`curl -sL kube-vip.io/k3s | vipAddress=192.168.0.10 vipInterface=ens192 sh | sudo tee /var/lib/rancher/k3s/server/manifests/vip.yaml`

### Start `k3s`

Set the VIP **first**

`export VIP=192.168.0.10`


From online `-->`

```
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--write-kubeconfig-mode 644 \
-t agent-secret --tls-san $VIP" sh -
```

From local `-->`

```
sudo ./k3s server --tls-san $VIP
```

#### With MySQL

From online `-->`

```
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--write-kubeconfig-mode 644 \
--datastore-endpoint mysql://root:k3s-password@tcp(192.168.0.43:3306)/kubernetes \
-t agent-secret --tls-san $VIP" sh -
```

From local `-->`

```
sudo ./k3s server --tls-san $VIP \
--datastore-endpoint="mysql://root:k3s-password@tcp(192.168.0.43:3306)/kubernetes"
```

### Get a `kubeconfig` that uses the vip

````
mkdir -p $HOME/.kube
sudo cat /etc/rancher/k3s/k3s.yaml | sed 's/127.0.0.1/'$VIP'/g' > $HOME/.kube/config
sudo chown $(id -u):$(id -g) $HOME/.kube/config
```
