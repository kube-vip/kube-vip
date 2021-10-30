# Kube-vip as a Static Pod

## Static Pods

Static pods are a Kubernetes pod that is ran by the `kubelet` on a single node, and is **not** managed by the Kubernetes cluster itself. This means that whilst the pod can appear within Kubernetes it can't make use of a variety of kubernetes functionality (such as the kubernetes token or `configMaps`). The static pod approach is primarily required for [kubeadm](https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/), this is due to the sequence of actions performed by `kubeadm`. Ideally we want `kube-vip` to be part of the kubernetes cluster, for various bits of functionality we also need `kube-vip` to provide a HA virtual IP as part of the installation. 

The sequence of events for this to work follows:
1. Generate a `kube-vip` manifest in the static pods manifest folder
2. Run `kubeadm init`, this generates the manifests for the control plane and wait to connect to the VIP
3. The `kubelet` will parse and execute all manifest, including the `kube-vip` manifest
4. `kube-vip` starts and advertises our VIP
5. The `kubeadm init` finishes succesfully.

## Kube-Vip as **HA**, **Load-Balancer** or both ` ¯\_(ツ)_/¯`

When generating a manifest for `kube-vip` we will pass in the flags `--controlplane` / `--services` these will enable the various types of functionality within `kube-vip`. 

With both enabled `kube-vip` will manage a virtual IP address that is passed through it's configuration for a Highly Available Kubernetes cluster, it will also "watch" services of `type:LoadBalancer` and once their `spec.LoadBalancerIP` is updated (typically by a cloud controller) it will advertise this address using BGP/ARP.

## Generating a Manifest

This section details creating a number of manifests for various use cases

### Set configuration details

`export VIP=192.168.0.40`

`export INTERFACE=<interface>`

## Configure to use a container runtime

The easiest method to generate a manifest is using the container itself, below will create an alias for different container runtimes.

### containerd
`alias kube-vip="ctr run --rm --net-host ghcr.io/kube-vip/kube-vip:v0.3.9 vip /kube-vip"`

### Docker
`alias kube-vip="docker run --network host --rm ghcr.io/kube-vip/kube-vip:v0.3.9"`


## ARP

This configuration will create a manifest that starts `kube-vip` providing **controlplane** and **services** management, using **leaderElection**. When this instance is elected as the leader it will bind the `vip` to the specified `interface`, this is also the same for services of `type:LoadBalancer`.

`export INTERFACE=eth0`

```
kube-vip manifest pod \
    --interface $INTERFACE \
    --vip $VIP \
    --controlplane \
    --services \
    --arp \
    --leaderElection | tee  /etc/kubernetes/manifests/kube-vip.yaml
```

## BGP

This configuration will create a manifest that will start `kube-vip` providing **controlplane** and **services** management. **Unlike** ARP, all nodes in the BGP configuration will advertise virtual IP addresses. 

**Note** we bind the address to `lo` as we don't want multiple devices that have the same address on public interfaces. We can specify all the peers in a comma seperate list in the format of `address:AS:password:multihop`.

`export INTERFACE=lo`

```
kube-vip manifest pod \
    --interface $INTERFACE \
    --vip $VIP \
    --controlplane \
    --services \
    --bgp \
    --localAS 65000 \
    --bgpRouterID 192.168.0.2 \
    --bgppeers 192.168.0.10:65000::false,192.168.0.11:65000::false | tee  /etc/kubernetes/manifests/kube-vip.yaml
```