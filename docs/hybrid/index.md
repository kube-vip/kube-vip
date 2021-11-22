# Using kube-vip in Hybrid Mode

We can deploy kube-vip in two different methods, which completely depends on your use-case and method for installing Kubernetes:

- Static Pods (hybrid)
- Daemonset (hybrid, requires taint)

## Prerequisites

In order for `kube-vip` to be able to speak with the Kubernetes API server, we need to be able to resolve the hostname within the pod. In order to ensure this will work as expected the `/etc/hosts` file should have the `hostname` of the server within it. The `/etc/hosts` file is passed into the running container and will ensure that the pod isn't "confused" by any Kubernetes networking.

## Kubernetes Services (`type:LoadBalancer`)

To learn more about how `kube-vip` in hybrid works with the LoadBalancer services within a kubernetes cluster the documentation is [here](./services/). To get `kube-vip` deployed read on!

## Static Pods

Static pods are a Kubernetes pod that is ran by the `kubelet` on a single node, and is **not** managed by the Kubernetes cluster itself. This means that whilst the pod can appear within Kubernetes it can't make use of a variety of kubernetes functionality (such as the kubernetes token or `configMaps`). The static pod approach is primarily required for [kubeadm](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/), this is due to the sequence of actions performed by `kubeadm`. Ideally we want `kube-vip` to be part of the kubernetes cluster, for various bits of functionality we also need `kube-vip` to provide a HA virtual IP as part of the installation.

The sequence of events for this to work follows:

1. Generate a `kube-vip` manifest in the static pods manifest folder
2. Run `kubeadm init`, this generates the manifests for the control plane and wait to connect to the VIP
3. The `kubelet` will parse and execute all manifest, including the `kube-vip` manifest
4. `kube-vip` starts and advertises our VIP
5. The `kubeadm init` finishes successfully.

## Daemonset

Other Kubernetes distributions can bring up a Kubernetes cluster, without depending on a VIP (BUT they are configured to support one). A prime example of this would be k3s, that can be configured to start and also sign the certificates to allow incoming traffic to a virtual IP. Given we don't need the VIP to exist **before** the cluster, we can bring up the k3s node(s) and then add `kube-vip` as a daemonset for all control plane nodes.

## Deploying `kube-vip`

The simplest method for generating the Kubernetes manifests is with `kube-vip` itself.. The subcommand `manifest pod|daemonset` can be used to generate specific types of Kubernetes manifests for use in a cluster. These subcommands can be configured with additional flags to enable/disable BGP/ARP/LeaderElection and a host of other options.

Both Examples will use the same Architecture:

## Infrastructure architecture

The infrastructure for our example HA Kubernetes cluster is as follows:

| Node           | Address   |
| -------------- | --------- |
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

## Kube-Vip flag reference

| Category           | Flag                   | Usage                                                              | Notes                                                                           |
| ------------------ | ---------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------------------- |
| **Mode**           |                        |                                                                    |                                                                                 |
|                    | `--controlPlane`       | Enables `kube-vip` control-plane functionality                     |                                                                                 |
|                    | `--services`           | Enables `kube-vip` to watch services of type:LoadBalancer          |                                                                                 |
| **Vip Config**     |                        |                                                                    |                                                                                 |
|                    | `--arp`                | Enables ARP broadcasts from Leader                                 |                                                                                 |
|                    | `--bgp`                | Enables BGP peering from `kube-vip`                                |                                                                                 |
|                    | `--vip`                | `<IP Address>`                                                     | (deprecated)                                                                    |
|                    | `--address`            | `<IP Address>` or `<DNS name>`                                     |                                                                                 |
|                    | `--interface`          | `<linux interface>`                                                |                                                                                 |
|                    | `--leaderElection`     | Enables Kubernetes LeaderElection                                  | Used by ARP, as only the leader can broadcast                                   |
| **Services**       |                        |                                                                    |                                                                                 |
|                    | `--cidr`               | Defaults "32"                                                      | Used when advertising BGP addresses (typically as `x.x.x.x/32`)                 |
| **Kubernetes**     |                        |                                                                    |                                                                                 |
|                    | `--inCluster`          | Defaults to looking inside the Pod for the token                   |                                                                                 |
|                    | `--taint`              | Enables a taint, stopping control plane daemonset being on workers |                                                                                 |
| **LeaderElection** |                        |                                                                    |                                                                                 |
|                    | `--leaseDuration`      | default 5                                                          | Seconds a lease is held for                                                     |
|                    | `--leaseRenewDuration` | default 3                                                          | Seconds a leader can attempt to renew the lease                                 |
|                    | `--leaseRetry`         | default 1                                                          | Number of times the leader will hold the lease for                              |
|                    | `--namespace`          | "kube-vip"                                                         | The namespace where the lease will reside                                       |
| **BGP**            |                        |                                                                    |                                                                                 |
|                    | `--bgpRouterID`        | `<IP Address>`                                                     | Typically the address of the local node                                         |
|                    | `--localAS`            | default 65000                                                      | The AS we peer from                                                             |
|                    | `--bgppeers`           | `<address:AS:password:multihop>`                                   | Comma separated list of BGP peers                                               |
|                    | `--peerAddress`        | `<IP Address>`                                                     | Address of a single BGP Peer                                                    |
|                    | `--peerAS`             | default 65000                                                      | AS of a single BGP Peer                                                         |
|                    | `--peerPass`           | ""                                                                 | Password to work with a single BGP Peer                                         |
|                    | `--multiHop`           | Enables eBGP MultiHop                                              | Enable multiHop with a single BGP Peer                                          |
|                    | `--annotations`        | `<provider string>`                                                | Startup will be paused until the node annotations contain the BGP configuration |
| **Equinix Metal**  |                        |                                                                    | (May be deprecated)                                                             |
|                    | `--metal`              | Enables Equinix Metal API calls                                    |                                                                                 |
|                    | `--metalKey`           | Equinix Metal API token                                            |                                                                                 |
|                    | `--metalProject`       | Equinix Metal Project (Name)                                       |                                                                                 |
|                    | `--metalProjectID`     | Equinix Metal Project (UUID)                                       |                                                                                 |
|                    | `--provider-config`    | Path to the Equinix Metal provider configuration                   | Requires the Equinix Metal CCM                                                  |

## Changelog

### Static DNS Support (added in 0.2.0)

A new flag `--address` is introduced to support using a DNS record as the control plane endpoint. `kube-vip` will do a dns lookup to retrieve the IP for the DNS record, and use that IP as the VIP. An `dnsUpdater` periodically checks and updates the system if IP changes for the DNS record.

### Dynamic DNS Support (added in 0.2.1)

`kube-vip` was also updated to support DHCP + [Dynamic DNS](https://en.wikipedia.org/wiki/Dynamic_DNS), for the use case where it's not able to reserve a static IP for the control plane endpoint.

A new flag `--ddns` is introduced. Once enabled, `kube-vip` expects the input `--address` will be a FQDN without binding to an IP. Then `kube-vip` will start a dhcp client to allocate an IP for the hostname of FQDN, and maintain the lease for it.

Once DHCP returns an IP for the FQDN, the same `dnsUpdater` runs to periodically checks and updates if IP got changed.

## BGP Support (added in 0.1.8)

In version `0.1.8` `kube-vip` was updated to support [BGP](https://en.wikipedia.org/wiki/Border_Gateway_Protocol) as a VIP failover mechanism. When a node is elected as a leader then it will update it's peers so that they are aware to route traffic to that node in order to access the VIP.

The following new flags are used:

- `--bgp` This will enable BGP support within kube-vip
- `--localAS` The local AS number
- `--bgpRouterID` The local router address
- `--peerAS` The AS number for a BGP peer
- `--peerAddress` The address of a BGP peer

### Equinix Metal BGP support

If the `--bgp` flag is passed along with the Equinix Metal flags `metal, metalKey and metalProject`, then Equinix Metal  API will be used in order to determine the BGP configuration for the nodes being used in the cluster. This automates a lot of the process and makes using BGP within Equinix Metal much simpler.

## Equinix Metal Control Plane Support (added in 0.1.8)

Recently in version `0.1.7` of `kube-vip` we added the functionality to use a Equinix Metal Elastic IP as the virtual IP fronting the Kubernetes Control plane cluster. In order to first get out virtual IP we will need to use our Equinix Metal account and create a EIP (either public or private). We will only need a single address so a `/32` will suffice, once this is created as part of a Equinix Metal project we can now apply this address to the servers that live in the same project.

In this example we've logged into the UI can created a new EIP of `147.75.1.2`, and we've deployed three small server instances with Ubuntu.

The following new flags are used:

- `--metal` which enables the use of the Equinix Metal API
- `--metalKey` which is our API key
- `--metalProject`which is the name of our Equinix Metal project where our servers and EIP are located.

*Also* the `--arp` flag should NOT be used as it wont work within the Equinix Metal network.
