# Kube-Vip Architecture

This section covers two parts of the architecture:

1. The technical capabilities of `kube-vip`.
2. The components to build a load balancing service within Kubernetes.

The `kube-vip` project is designed to provide both a highly available networking endpoint and load balancing functionality for underlying networking services. The project was originally designed for the purpose of providing a resilient control plane for Kubernetes but has since expanded to provide the same functionality for Service resources within a Kubernetes cluster.

Additionally, `kube-vip` is designed to be lightweight and multi-architecture. All of the components are built for Linux on `x86`, `armv7`, `armhvf`, `ppc64le` and `s390x` architectures. This means that `kube-vip` will run fine in bare metal, virtual, and edge (Raspberry Pi or small ARM SoC) use cases.

## Technologies

There are a number of technologies or functional design choices that provide high availability and networking functions as part of a VIP/load balancing solution.

### Cluster

The `kube-vip` service builds a multi-node or multi-pod cluster to provide high availability. In ARP mode, a leader is elected which will inherit the virtual IP and become the leader of the load balancing within the cluster whereas with BGP all nodes will advertise the VIP address.

When using ARP or [Layer 2](https://osi-model.com/data-link-layer/) it will use [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection).

### Virtual IP

The leader within the cluster will assume the VIP and will have it bound to the selected interface that is declared within the configuration. When the leader changes, it will evacuate the VIP first or in failure scenarios the VIP will be directly assumed by the next elected leader.

When the VIP moves from one host to another, any host that has been using the VIP will retain the previous VIP-to-MAC address mapping until the old ARP entry expires (typically within 30 seconds) and retrieves a new mapping. This can be improved by using [Gratuitous ARP](https://wiki.wireshark.org/Gratuitous_ARP) broadcasts when enabled (detailed below).

### ARP

`kube-vip` can optionally be configured to broadcast a Gratuitous ARP that will typically immediately notify all local hosts that the VIP-to-MAC address mapping has changed.

Below we can see that the failover is typically done within a few seconds as the ARP broadcast is received.

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

### Load Balancing

`kube-vip` has the capability to provide a high availability address for both the Kubernetes control plane and for a Kubernetes Service. As of v0.4.0, `kube-vip` implements support for true load balancing for the control plane to distribute API requests across control plane nodes.

#### Kubernetes Service Load Balancing

The following is required in the `kube-vip` manifest to enable Service of type `LoadBalancer`:

```yaml
- name: svc_enable
  value: "true"
```

This section details the flow of events in order for `kube-vip` to advertise a Kubernetes Service:

1. An end user exposes an application through Kubernetes as a Service type `LoadBalancer`. For example, imperatively using `kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx`
2. Within the Kubernetes cluster, a Service object is created with the `spec.type` set to `LoadBalancer`.
3. A controller (typically a [Cloud Controller](/usage/cloud-provider)) has a loop that "watches" for Services of the type `LoadBalancer`.
4. The controller now has the responsibility of providing an IP address for this Service along with doing anything that is network specific for the environment where the cluster is running.
5. Once the controller has an IP address, it will update the Service field `spec.annotations["kube-vip.io/loadbalancerIPs"]` and `spec.loadBalancerIP` with the IP address. `spec.loadBalancerIP` is deprecated in k8s 1.24, will not be updated in future release
6. `kube-vip` Pods implement a "watcher" for Services that have a `spec.annotations["kube-vip.io/loadbalancerIPs"]` address attached. If the annotation is not presented, it will fallback to check `svc.Spec.loadBalancerIP`.
7. When a new Service appears, `kube-vip` will start advertising this address to the wider network (through BGP/ARP) which will allow traffic to come into the cluster and hit the Service network.
8. Finally, `kube-vip` will update the Service status so that the API reflects the object is ready. This is done by updating the `status.loadBalancer.ingress` with the VIP address.

#### Control Plane Load-Balancing

As of `kube-vip` v0.4.0, IPVS load balancing is configured for having the VIP in the same subnet as the control plane nodes. NAT-based load balancing will follow later.

To enable control plane load balancing using IPVS, the environment variable `lb_enable` is required in the `kube-vip` manifest:

```yaml
- name : lb_enable
  value: "true"
```

The load balancing is provided through IPVS (IP Virtual Server) and provides a Layer 4 (TCP-based) round-robin across all of the control plane nodes. By default, the load balancer will listen on the default port of 6443 as the Kubernetes API server. The IPVS virtual server lives in kernel space and doesn't create an "actual" service that listens on port 6443. This allows the kernel to parse packets before they're sent to an actual TCP port. This is important to know because it means we don't have any port conflicts having the IPVS load balancer listening on the same port as the API server on the same host.

The load balancer port can be customised by changing the `lb_port` environment variable in the `kube-vip` manifest:

```yaml
- name: lb_port
  value: "6443"
```

##### How it works

Once the `lb_enable` variable is set to `true`, `kube-vip` will do the following:

- In Layer 2 it will create an IPVS service on the leader.
- In Layer 3 all nodes will create an IPVS service.
- It will start a Kubernetes node watcher for nodes with the control plane label.
- It will add/delete them as they're added and removed from the cluster.

#### Debugging control plane load balancing

In order to inspect and debug traffic, install the `ipvsadm` tool.

##### View the configuration

The command `sudo ipvsadm -ln` will display the load balancer configuration.

```sh
$ sudo ipvsadm -ln
IP Virtual Server version 1.2.1 (size=4096)
Prot LocalAddress:Port Scheduler Flags
-> RemoteAddress:Port           Forward Weight ActiveConn InActConn
TCP  192.168.0.40:6443 rr
-> 192.168.0.41:6443            Local   1      4          0
-> 192.168.0.42:6443            Local   1      3          0
-> 192.168.0.43:6443            Local   1      3          0
```

##### Watch things interact with the API server

The command `watch sudo ipvsadm -lnc` will auto-refresh the connections to the load balancer.

```sh
$ watch sudo ipvsadm -lnc

<snip>

sudo ipvsadm -lnc                    k8s01: Tue Nov  9 11:39:39 2021

IPVS connection entries
pro expire state       source             virtual            destination
TCP 14:49  ESTABLISHED 192.168.0.42:37090 192.168.0.40:6443  192.168.0.41:6443
TCP 14:55  ESTABLISHED 192.168.0.45:46510 192.168.0.40:6443  192.168.0.41:6443
TCP 14:54  ESTABLISHED 192.168.0.43:39602 192.168.0.40:6443  192.168.0.43:6443
TCP 14:58  ESTABLISHED 192.168.0.44:50458 192.168.0.40:6443  192.168.0.42:6443
TCP 14:32  ESTABLISHED 192.168.0.43:39648 192.168.0.40:6443  192.168.0.42:6443
TCP 14:58  ESTABLISHED 192.168.0.40:55944 192.168.0.40:6443  192.168.0.41:6443
TCP 14:54  ESTABLISHED 192.168.0.42:36950 192.168.0.40:6443  192.168.0.41:6443
TCP 14:42  ESTABLISHED 192.168.0.44:50488 192.168.0.40:6443  192.168.0.43:6443
TCP 14:53  ESTABLISHED 192.168.0.45:46528 192.168.0.40:6443  192.168.0.43:6443
TCP 14:49  ESTABLISHED 192.168.0.40:56040 192.168.0.40:6443  192.168.0.42:6443
```

## Components within a Kubernetes Cluster

The `kube-vip` Kubernetes load balancer requires a number of components in order to function:

- [Kube-Vip Cloud Provider](https://github.com/kube-vip/kube-vip-cloud-provider)
- [Kube-Vip Deployment](https://github.com/kube-vip/kube-vip)
