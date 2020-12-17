# Kube-vip

A Load-Balancer for both **inside** and **outside** a Kubernetes cluster

![kube-vip.png](kube-vip.png)

## Architecture

The architecture for `kube-vip` (and associated kubernetes components) is covered in detail [here](/architecture/)


## (New) Hybrid- Control-plane HA and Kubernetes service `type=LoadBalancer`

With the newest release of `kube-vip` the internal "manager" can handle the lifecycle of VIPs for both HA and for Kubernetes Load-Balancing. The main driver for this is being most effective for large nodes that can run control-plane components and run applications. The details for hybrid mode are [here](/hybrid/)

## (Legacy) Control-plane load balancer

The details are [here](/control-plane/)

## (Legacy) Kubernetes service `"type: LoadBalancer"`

The details are [here](/kubernetes/)

## GitHub Repositories

- The Plunder Cloud Provider -> [https://github.com/plunder-app/plndr-cloud-provider](https://github.com/plunder-app/plndr-cloud-provider)
- The Kube-Vip Deployment -> [https://github.com/plunder-app/kube-vip](https://github.com/plunder-app/kube-vip)