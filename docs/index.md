![kube-vip.png](kube-vip.png)

## Overview
Kubernetes Virtual IP and Load-Balancer for both control plane and Kubernetes services

The idea behind `kube-vip` is a small self-contained Highly-Available option for all environments, especially:

- Bare-Metal
- On-Prem
- Edge (ARM / Raspberry PI)
- Virtualisation
- Pretty much anywhere else :)

## Features

Kube-Vip was originally created to provide a HA solution for the Kubernetes control plane, over time it has evolved to incorporate that same functionality into Kubernetes service type [load-balancers](https://kubernetes.io/docs/concepts/services-networking/service/#loadbalancer).

- VIP addresses can be both IPv4 or IPv6
- Control Plane with ARP (Layer 2) or BGP (Layer 3)
- Control Plane using either [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection) or [raft](https://en.wikipedia.org/wiki/Raft_(computer_science))
- Control Plane HA with kubeadm (static Pods)
- Control Plane HA with K3s/and others (daemonsets)
- Control Plane LoadBalancing with IPVS (kube-vip > 0.4)
- Service LoadBalancer using [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection) for ARP (Layer 2)
- Service LoadBalancer using multiple nodes with BGP
- Service LoadBalancer address pools per namespace or global
- Service LoadBalancer address via (existing network DHCP)
- Service LoadBalancer address exposure to gateway via UPNP
- ... manifest generation, vendor API integrations and many nore... 

## Why?

The "original" purpose of `kube-vip` was to simplify the building of HA Kubernetes clusters, which at this time can involve a few components and configurations that all need to be managed. This was blogged about in detail by [thebsdbox](https://twitter.com/thebsdbox/) here -> [https://thebsdbox.co.uk/2020/01/02/Designing-Building-HA-bare-metal-Kubernetes-cluster/#Networking-load-balancing](https://thebsdbox.co.uk/2020/01/02/Designing-Building-HA-bare-metal-Kubernetes-cluster/#Networking-load-balancing). As the project evolved it now can use those same technologies to provide load-balancing capabilities within a Kubernetes Cluster.


## Architecture

The architecture for `kube-vip` (and associated kubernetes components) is covered in detail [here](/architecture/)

## Installation

There are two main routes for deploying `kube-vip`, either through a [static pod](https://kubernetes.io/docs/tasks/configure-pod-container/static-pod/) when bringing up a Kubernetes cluster with [kubeadm](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/) or as a [daemon set](https://kubernetes.io/docs/concepts/workloads/controllers/daemonset/) (typically with distributions like [k3s](https://k3s.io)). 

The infrastructure for our example HA Kubernetes cluster is as follows:

| Node           | Address    |
|----------------|------------|
| VIP            | 10.0.0.40 |
| controlPlane01 | 10.0.0.41 |
| controlPlane02 | 10.0.0.42 |
| controlPlane03 | 10.0.0.43 |
| worker01       | 10.0.0.44 |

All nodes are running Ubuntu 20.04, Docker CE and will use Kubernetes 1.21.0, we only have one worker as we're going to use our controlPlanes in "hybrid" mode.

- [Static Pod](/install_static)
- [Daemon Set](/install_daemonset)

## Usage

- [On-Prem with the kube-vip cloud controller](/usage/on-prem)
- [KIND](/usage/kind)
- [Equinix Metal](/usage/EquinixMetal)
- [k3s](/usage/k3s)

## Flags/Environment Variables

- [Flags and Environment variables](/flags/)

## Links

- The Kube-Vip Cloud Provider Repository -> [https://github.com/kube-vip/kube-vip-cloud-provider](https://github.com/kube-vip/kube-vip-cloud-provider)
- The Kube-Vip Repository -> [https://github.com/kube-vip/kube-vip](https://github.com/kube-vip/kube-vip)
- The Kube-Vip RBAC (required for the daemonset) -> [https://kube-vip.io/manifests/rbac.yaml](https://kube-vip.io/manifests/rbac.yaml)
## Copyright

© 2021 [The Linux Foundation](https://www.linuxfoundation.org/). All right reserved

The Linux Foundation has registered trademarks and uses trademarks.

For a list trademarks of The Linux Foundation, please see our [Trademark Usage page](https://www.linuxfoundation.org/en/trademark-usage).
