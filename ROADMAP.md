# Kube-Vip Roadmap

This document outlines the roadmap for the **kube-vip** project and only covers the technologies within this particular project, other projects that augment or provide additional functionality (such as cloud-providers) may have their own roadmaps in future. The functionality for **kube-vip** has grown either been developed organically or through real-world needs, and this is the first attempt to put into words a plan for the future of **kube-vip** and will additional evolve over time. This means that items listed or detailed here are not necessarily set in stone and the roadmap can grow/shrink as the project matures. We definitely welcome suggestions and ideas from everyone about the roadmap and **kube-vip** features. Reach us through Issues, Slack or email <catch-all>@kube-vip.io.

## Release methodology

The **kube-vip** project attempts to follow a tick-tock release cycle, this typically means that one release will come **packed** with new features where the following release will come with fixes, code sanitation and performance enhancements.

## Roadmap

The **kube-vip** project offers two main areas of functionality:

- HA Kubernetes clusters through a control-plane VIP
- Kubernetes `service type:LoadBalancer`

Whilst both of these functions share underlying technologies and code they will have slightly differing roadmaps.

### HA Kubernetes Control Plane

- **Re-implement LoadBalancing** - due to a previous request the HTTP loadbalancing was removed leaving just HA for the control plane. This functionality will be re-implemented either through the original round-robin HTTP requests or utilising IPVS.
- **Utilise the Kubernetes API to determine additional Control Plane members** - Once a single node cluster is running **kube-vip** could use the API to determine the additional members, at this time a Cluster-API provider needs to drop a static manifest per CP node.
- **Re-evaluate raft** - **kube-vip** is mainly designed to run within a Kubernetes cluster, however it's original design was a raft cluster external to Kubernetes. Unfortunately given some of the upgrade paths identified in things like CAPV moving to leaderElection within Kubernetes became a better idea.

## Kubernetes `service type:LoadBalancer`

- **`ARP` LeaderElection per loadBalancer** - Currently only one pod that is elected leader will field all traffic for a VIP.. extending this to generate a leaderElection token per service would allow services to proliferate across all pods across the cluster
- **Aligning of `service` and `manager`** - The move to allow hybrid (be both HA control plane and offer load-balancer services at the same time) introduced a duplicate code path.. these need to converge as it's currently confusing for contributors.

## Global **Kube-Vip** items

- **Improved metrics** - At this time the scaffolding for monitoring exists, however this needs drastically extending to provide greater observability to what is happening within **kube-vip**
- **Windows support** - The Go SDK didn't support the capability for low-levels sockets for ARP originally, this should be revisited.
- **Additional BGP features** :
  - Communities
  - BFD
