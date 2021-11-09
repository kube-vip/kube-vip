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

### Get latest version

 We can parse the GitHub API to find the latest version (or we can set this manually)

`KVVERSION=$(curl -sL https://api.github.com/repos/kube-vip/kube-vip/releases | jq -r ".[0].name")`

or manually:

`export KVVERSION=vx.x.x`

The easiest method to generate a manifest is using the container itself, below will create an alias for different container runtimes.

### containerd
`alias kube-vip="ctr run --rm --net-host ghcr.io/kube-vip/kube-vip:$KVVERSION vip /kube-vip"`

### Docker
`alias kube-vip="docker run --network host --rm ghcr.io/kube-vip/kube-vip:$KVVERSION"`

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

### Example manifest

```
apiVersion: v1
kind: Pod
metadata:
  creationTimestamp: null
  name: kube-vip
  namespace: kube-system
spec:
  containers:
  - args:
    - manager
    env:
    - name: vip_arp
      value: "true"
    - name: port
      value: "6443"
    - name: vip_interface
      value: ens192
    - name: vip_cidr
      value: "32"
    - name: cp_enable
      value: "true"
    - name: cp_namespace
      value: kube-system
    - name: vip_ddns
      value: "false"
    - name: svc_enable
      value: "true"
    - name: vip_leaderelection
      value: "true"
    - name: vip_leaseduration
      value: "5"
    - name: vip_renewdeadline
      value: "3"
    - name: vip_retryperiod
      value: "1"
    - name: vip_address
      value: 192.168.0.40
    image: ghcr.io/kube-vip/kube-vip:v0.3.9
    imagePullPolicy: Always
    name: kube-vip
    resources: {}
    securityContext:
      capabilities:
        add:
        - NET_ADMIN
        - NET_RAW
        - SYS_TIME
    volumeMounts:
    - mountPath: /etc/kubernetes/admin.conf
      name: kubeconfig
  hostAliases:
  - hostnames:
    - kubernetes
    ip: 127.0.0.1
  hostNetwork: true
  volumes:
  - hostPath:
      path: /etc/kubernetes/admin.conf
    name: kubeconfig
status: {}
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

### Example Manifest 

```
apiVersion: v1
kind: Pod
metadata:
  creationTimestamp: null
  name: kube-vip
  namespace: kube-system
spec:
  containers:
  - args:
    - manager
    env:
    - name: vip_arp
      value: "false"
    - name: port
      value: "6443"
    - name: vip_interface
      value: ens192
    - name: vip_cidr
      value: "32"
    - name: cp_enable
      value: "true"
    - name: cp_namespace
      value: kube-system
    - name: vip_ddns
      value: "false"
    - name: bgp_enable
      value: "true"
    - name: bgp_routerid
      value: 192.168.0.2
    - name: bgp_as
      value: "65000"
    - name: bgp_peeraddress
    - name: bgp_peerpass
    - name: bgp_peeras
      value: "65000"
    - name: bgp_peers
      value: 192.168.0.10:65000::false,192.168.0.11:65000::false
    - name: vip_address
      value: 192.168.0.40
    image: ghcr.io/kube-vip/kube-vip:v0.3.9
    imagePullPolicy: Always
    name: kube-vip
    resources: {}
    securityContext:
      capabilities:
        add:
        - NET_ADMIN
        - NET_RAW
        - SYS_TIME
    volumeMounts:
    - mountPath: /etc/kubernetes/admin.conf
      name: kubeconfig
  hostAliases:
  - hostnames:
    - kubernetes
    ip: 127.0.0.1
  hostNetwork: true
  volumes:
  - hostPath:
      path: /etc/kubernetes/admin.conf
    name: kubeconfig
status: {}
```