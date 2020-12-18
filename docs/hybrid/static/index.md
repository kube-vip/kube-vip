# Kube-vip as a Static Pod

In Hybrid mode `kube-vip` will manage a virtual IP address that is passed through it's configuration for a Highly Available Kubernetes cluster, it will also "watch" services of `type:LoadBalancer` and once their `spec.LoadBalancerIP` is updated (typically by a cloud controller) it will advertise this address using BGP/ARP.

The "hybrid" mode is now the default mode in `kube-vip` from `0.2.3` onwards, and allows both modes to be enabled at the same time. 

## Generating a Manifest

This section details creating a number of manifests for various use cases

### Set configuration details

`export VIP=192.168.0.40`

`export INTERFACE=<interface>`

### Configure to use a container runtime

The easiest method to generate a manifest is using the container itself, below will create an alias for different container runtimes.

#### containerd
`alias kube-vip="ctr run --rm --net-host docker.io/plndr/kube-vip:0.2.3 vip /kube-vip"`

#### Docker
`alias kube-vip="docker run --network host --rm plndr/kube-vip:0.2.3"`


### ARP

This configuration will create a manifest that starts `kube-vip` providing **controlplane** and **services** management, using **leaderElection**. When this instance is elected as the leader it will bind the `vip` to the specified `interface`, this is also the same for services of `type:LoadBalancer`.

`export INTERFACE=eth0`

```
kube-vip manifest pod \
    --interface $INTERFACE \
    --vip $VIP \
    --controlplane \
    --services \
    --arp \
    --leaderElection
```

### BGP

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
    --bgppeers 192.168.0.10:65000::false,192.168.0.11:65000::false
```

### BGP with Packet

We can enable `kube-vip` with the capability to pull all of the required configuration for BGP by passing the `--packet` flag and the API Key and our project ID.

```
kube-vip manifest pod \
    --interface $INTERFACE\
    --vip $VIP \
    --controlplane \
    --services \
    --bgp \
    --packet \
    --packetAPI xxxxxxx \ 
    --packetProjectID xxxxx
```

## Deploy your Kubernetes Cluster

### First node

```
sudo kubeadm init \
    --kubernetes-version 1.19.0 \
    --control-plane-endpoint $VIP \
    --upload-certs
```

### Additional Node(s)

Due to an oddity with `kubeadm` we can't have our `kube-vip` manifest present **before** joining our additional nodes. So on these control plane nodes we will add them first to the cluster.

```
sudo kubeadm join $VIP:6443 \
    --token w5atsr.blahblahblah 
    --control-plane \
    --certificate-key abc123
```

**Once**, joined these nodes can have the same command that we ran on the first node to populate the `/etc/kubernetes/manifests/` folder with the `kube-vip` manifest.

## Services

At this point your `kube-vip` static pods will be up and running and where used with the `--services` flag will also be watching for Kubernetes services that they can advertise. In order for `kube-vip` to advertise a service it needs a CCM or other controller to apply an IP address to the `spec.LoadBalancerIP`, which marks the loadbalancer as defined. 
