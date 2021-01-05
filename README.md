# kube-vip

High Availability and Load-Balancing 

![](https://kube-vip.io/kube-vip.png)

## Overview
Kubernetes Virtual IP and Load-Balancer for both control plane and Kubernetes services

The idea behind `kube-vip` is a small self-contained Highly-Available option for all environments, especially:

- Bare-Metal
- Edge (arm / Raspberry PI)
- Virtualisation
- Pretty much anywhere else :)

**NOTE** All documentation of both usage and architecture are now available at [https://kube-vip.io](https://kube-vip.io)

## Features

Kube-Vip was originally created to provide a HA solution for the Kubernetes control plane, over time it has evolved to incorporate that same functionality into Kubernetes service type [load-balancers](https://kubernetes.io/docs/concepts/services-networking/service/#loadbalancer).

- Control Plane with ARP (Layer 2) or BGP (Layer 3)
- Control Plane using either [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection) or [raft](https://en.wikipedia.org/wiki/Raft_(computer_science))
- Control Plane HA with kubeadm (static Pods)
- Control Plane HA with K3s/and others (daemonsets)
- Service LoadBalancer using [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection) for ARP (Layer 2)
- Service LoadBalancer using multiple nodes with BGP
- Service LoadBalancer address pools per namespace or global
- Service LoadBalancer address via (existing network DHCP)
- Service LoadBalancer address exposure to gateway via UPNP
- ... manifest generation, vendor API integrations and many nore... 

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
- Hardware Load-balancer (functionality differs per vendor)

All of these would require a separate level of configuration and in some infrastructures multiple teams in order to implement. Also when considering the software components, they may require packaging into containers or if they’re pre-packaged then security and transparency may be an issue. Finally, in edge environments we may have limited room for hardware (no HW load-balancer) or packages solutions in the correct architectures might not exist (e.g. ARM). Luckily with `kube-vip` being written in GO, it’s small(ish) and easy to build for multiple architectures, with the added security benefit of being the only thing needed in the container.

## Troubleshooting and Feedback

Please raise issues on the GitHub repository and as mentioned check the documentation at [https://kube-vip.io](https://kube-vip.io/)
