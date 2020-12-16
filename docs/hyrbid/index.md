# Using `kube-vip` in Hybrid Mode

We can deploy kube-vip in two different methods, which completely depends on your use-case and method for installing Kubernetes:

- Static Pods (hybrid)
- Daemonset (services only)

## Static Pods

Static pods are a Kubernetes pod that is ran by the `kubelet` on a single node, and is **not** managed by the Kubernetes cluster itself. This means that whilst the pod can appear within Kubernetes it can't make use of a variety of kubernetes functionality (such as the kubernetes token or `configMaps`). The static pod approach is primarily required for [kubeadm](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/), this is due to the sequence of actions performed by `kubeadm`. Ideally we want `kube-vip` to be part of the kubernetes cluster, for various bits of functionality we also need `kube-vip` to provide a HA virtual IP as part of the installation. 

The sequence of events for this to work follows:
1. Generate a `kube-vip` manifest in the static pods manifest folder
2. Run `kubeadm init`, this generates the manifests for the control plane and wait to connect to the VIP
3. The `kubelet` will parse and execute all manifest, including the `kube-vip` manifest
4. `kube-vip` starts and advertises our VIP
5. The `kubeadm init` finishes succesfully.

## Daemonset

Other Kubernetes distributions can bring up a Kubernetes cluster, without depending on a VIP (BUT they are configured to support one). A prime example of this would be k3s, that can be configured to start and also sign the certificates to allow incoming traffic to a virtual IP. Given we don't need the VIP to exist **before** the cluster, we can bring up the k3s node(s) and then add `kube-vip` as a daemonset for all control plane nodes.

# Deploying `kube-vip`

The simplest method for generating the Kubernetes manifests is with `kube-vip` itself.. The subcommand `manifest pod|daemonset` can be used to generate specific types of Kubernetes manifests for use in a cluster. These subcommands can be configured with additional flags to enable/disable BGP/ARP/LeaderElection and a host of other options.

Both Examples will use the same Architecture:

#### Infrastructure architecture

The infrastructure for our example HA Kubernetes cluster is as follows:

| Node           | Address    |
|----------------|------------|
| VIP            | 10.0.0.40 |
| controlPlane01 | 10.0.0.41 |
| controlPlane02 | 10.0.0.42 |
| controlPlane03 | 10.0.0.43 |
| worker01       | 10.0.0.44 |

All nodes are running Ubuntu 18.04, Docker CE and will use Kubernetes 1.19.0, we only have one worker as we're going to use our controlPlanes in "hybrid" mode.

## As a static Pod (for kubeadm)

The details for creating a static pod are available [here](./static/)

## As a daemonset

When using `kube-vip` as a daemonset the details are available [here](./daemonset/)

# Kube-Vip flag reference

| Category     | Flag | Usage | Notes |
|--------------|------|-------|-------|
|**Mode**          ||||
|              |`--controlPlane`|Enables `kube-vip` control-plane functionality||
|              |`--servcies`|Enables `kube-vip` to watch services of type:LoadBalancer||
|**Vip Config**   ||||
|              |`--arp`|Enables ARP brodcasts from Leader||
|              |`--bgp`|Enables BGP peering from `kube-vip`||
|              |`--vip`|\<IP Address>|(deprecated)|
|              |`--address`|\<IP Address> or \<DNS name>||
|              |`--interface`|\<linux interface>||
|              |`--leaderElection`|Enables Kubernetes LeaderElection|Used by ARP, as only the leader can broadcast|
|**Services**||||
|              |`--cidr`|Defaults "32"|Used when advertising BGP addresses (typically as `x.x.x.x/32`)|
|**Kubernetes**||||
|              |`--inCluster`|Defaults to looking inside the Pod for the token||
|**LeaderElection**||||
|              |`--leaseDuration`|default 5|Seconds a lease is held for|
|              |`--leaseRenewDuration`|default 3|Seconds a leader can attempt to renew the lease|
|              |`--leaseRetry`|default 1|Number of times the leader will hold the lease for|
|              |`--namespace`|"kube-vip"|The namespace where the lease will reside|
|**BGP**||||
|              |`--bgpRouterID`|\<IP Address>|Typically the address of the local node|
|              |`--localAS`|default 65000|The AS we peer from|
|              |`--bgppeers`|`<address:AS:password:mutlihop>`|Comma seperate list of BGP peers|
|              |`--peerAddress`|\<IP Address>|(deprecated), Address of a single BGP Peer|
|              |`--peerAS`|default 65000|(deprecated), AS of a single BGP Peer|
|              |`--peerPass`|""|(deprecated), Password to work with a single BGP Peer|
|              |`--multiHop`|Enables eBGP MultiHop|(deprecated), Enable multiHop with a single BGP Peer|
|**Packet**|||(To be deprecated)|
|              |`--packet`|Enables Packet API calls||
|              |`--packetKey`|Packet API token||
|              |`--packetProject`|Packet Project (Name)||
|              |`--packetProjectID`|Packet Project (UUID)||
|              |`--provider-config`|Path to the Packet provider configuration|Requires the Packet CCM|