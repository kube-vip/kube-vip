![kube-vip.png](kube-vip.png)

## Overview

Kube-Vip provides Kubernetes clusters a virtual IP and load balancer for both control plane and Kubernetes Services.

The idea behind `kube-vip` is a small, self-contained, highly-available option for all environments, especially:

- Bare metal
- On-Premises
- Edge (ARM / Raspberry Pi)
- Virtualisation
- Pretty much anywhere else :)

## Features

Kube-Vip was originally created to provide a HA solution for the Kubernetes control plane, but over time it has evolved to incorporate that same functionality for Kubernetes Services of type [LoadBalancer](https://kubernetes.io/docs/concepts/services-networking/service/#loadbalancer). Some of the features include:

- VIP addresses can be both IPv4 or IPv6
- Control Plane with ARP (Layer 2) or BGP (Layer 3)
- Control Plane using either [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection) or [raft](https://en.wikipedia.org/wiki/Raft_(computer_science))
- Control Plane HA with kubeadm (static Pods)
- Control Plane HA with K3s/and others (DaemonSets)
- Control Plane LoadBalancing with IPVS (kube-vip ≥ 0.4)
- Service LoadBalancer using [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection) for ARP (Layer 2)
- Service LoadBalancer using multiple nodes with BGP
- Service LoadBalancer address pools per namespace or global
- Service LoadBalancer address via (existing network DHCP)
- Service LoadBalancer address exposure to gateway via UPnP
- ... manifest generation, vendor API integrations and many more...

## Why?

The "original" purpose of `kube-vip` was to simplify the building of HA Kubernetes clusters, which at the time involved a few components and configurations that all needed to be managed. This was blogged about in detail by [thebsdbox](https://twitter.com/thebsdbox/) [here](https://thebsdbox.co.uk/2020/01/02/Designing-Building-HA-bare-metal-Kubernetes-cluster/#Networking-load-balancing). Since the project has evolved, it can now use those same technologies to provide load balancing capabilities within a Kubernetes Cluster.

## Architecture

The architecture for `kube-vip` (and associated Kubernetes components) is covered in detail [here](/architecture/).

## Installation

There are two main routes for deploying `kube-vip`: either through a [static pod](https://kubernetes.io/docs/tasks/configure-pod-container/static-pod/) when bringing up a Kubernetes cluster with [kubeadm](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/) or as a [DaemonSet](https://kubernetes.io/docs/concepts/workloads/controllers/daemonset/) (typically with distributions like [K3s](https://k3s.io)).

The infrastructure for our example HA Kubernetes cluster is as follows:

| Node           | Address    |
|----------------|------------|
| VIP            | 10.0.0.40 |
| controlPlane01 | 10.0.0.41 |
| controlPlane02 | 10.0.0.42 |
| controlPlane03 | 10.0.0.43 |
| worker01       | 10.0.0.44 |

All nodes are running Ubuntu 20.04, Docker CE and will use Kubernetes 1.21.0. We only have one worker as we're going to use our control plane in "hybrid" mode.

- [Static Pod](/install_static)
- [DaemonSet](/install_daemonset)

## Usage

- [On-Prem with the kube-vip cloud controller](/usage/on-prem)
- [KIND](/usage/kind)
- [Equinix Metal](/usage/EquinixMetal)
- [k3s](/usage/k3s)

## Flags/Environment Variables

- [Flags and Environment variables](/flags/)

## Links

- [Kube-Vip Cloud Provider Repository](https://github.com/kube-vip/kube-vip-cloud-provider)
- [Kube-Vip Repository](https://github.com/kube-vip/kube-vip)
- [Kube-Vip RBAC manifest (required for the DaemonSet)](https://kube-vip.io/manifests/rbac.yaml)

## Copyright

© 2021 [The Linux Foundation](https://www.linuxfoundation.org/). All rights reserved.

The Linux Foundation has registered trademarks and uses trademarks.

For a list trademarks of The Linux Foundation, please see our [Trademark Usage page](https://www.linuxfoundation.org/en/trademark-usage).
