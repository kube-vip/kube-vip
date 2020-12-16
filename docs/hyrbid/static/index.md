# Kube-vip as a Static Pod

In Hybrid mode `kube-vip` will manage a virtual IP address that is passed through it's configuration for a Highly Available Kubernetes cluster, it will also "watch" services of `type:LoadBalancer` and once their `spec.LoadBalancerIP` is updated (typically by a cloud controller) it will advertise this address using BGP/ARP.

The "hybrid" mode is now the default mode in `kube-vip` from `0.2.3` onwards, and allows both modes to be enabled at the same time. 

## Generating a Manifest

This section details creating a number of manifests for various use cases

## Configure to use a container runtime

The easiest method to generate a manifest is using the container itself, below will create an environmemnt variable for different container runtimes.

### containerd
`export KUBE-VIP="ctr run --rm --net-host docker.io/plndr/kube-vip:0.2.3 vip"`

### Docker
`export KUBE-VIP="docker run --network host --rm plndr/kube-vip:0.2.3"`

### ARP

This configuration will create a manifest that starts `kube-vip` providing **controlplane** and **services** management, using **leaderElection**. When this instance is elected as the leader it will bind the `vip` to the specified `interface`, this is also the same for services of `type:LoadBalancer`.
```
$KUBE-VIP kube-vip manifest pod \
    --interface eth0 \
    --vip 192.168.0.40 \
    --controlplane \
    --services \
    --arp \
    --leaderElection
```

### BGP

This configuration will create a manifest that will start `kube-vip` providing **controlplane** and **services** management. **Unlike** ARP, all nodes in the BGP configuration will advertise virtual IP addresses. 

**Note** we bind the address to `lo` as we don't want multiple devices that have the same address on public interfaces. We can specify all the peers in a comma seperate list in the format of `address:AS:password:multihop`.

```
$KUBE-VIP kube-vip manifest pod \
    --interface lo \
    --vip 192.168.0.40 \
    --controlplane \
    --services \
    --bgp \
    --bgppeers 192.168.0.10:65000::false,192.168.0.11:65000::false
```

### BGP with Packet

We can enable `kube-vip` with the capability to pull all of the required configuration for BGP by passing the `--packet` flag and the API Key and our project ID.

```
$KUBE-VIP kube-vip manifest pod \
    --interface lo \
    --vip 192.168.0.40 \
    --controlplane \
    --services \
    --bgp \
    --packet \
    --packetAPI xxxxxxx \ 
    --packetProjectID xxxxx
```

## Services

At this point your `kube-vip` static pods will be up and running and where passed with the `--services` flag will also be watching for Kubernetes services that they can advertise. In order for `kube-vip` to advertise a service it needs a CCM or other controller to apply an IP address to the `spec.LoadBalancerIP`, which marks the loadbalancer as defined. 