# Kube-Vip as a daemonset

In Hybrid mode `kube-vip` will manage a virtual IP address that is passed through it's configuration for a Highly Available Kubernetes cluster, it will also "watch" services of `type:LoadBalancer` and once their `spec.LoadBalancerIP` is updated (typically by a cloud controller) it will advertise this address using BGP/ARP.


**Note about Daemonsets**

The "hybrid" mode is now the default mode in `kube-vip` from `0.2.3` onwards, and allows both modes to be enabled at the same time. 

If the Kubernetes installer allows for adding a Virtual IP as an additional [SAN](https://en.wikipedia.org/wiki/Subject_Alternative_Name) to the API server certificate then we can apply `kube-vip` to the cluster once the first node has been brought up. 

Unlike generating the static manifest there are a few more things that may need configuring, this page will cover most scenarios.

## Create the RBAC settings

As a daemonSet runs within the Kubernetes cluster it needs the correct access to be able to watch Kubernetes services and other objects. In order to do this we create a User, Role, and a binding.. we can apply this with the command:

```
kubectl apply -f https://kube-vip.io/manifests/rbac.yaml
```

## Generating a Manifest

This section only covers generating a simple *BGP* configuration, as the main focus is will be on additional changes to the manifest. For more examples we can look at [here](/hybrid/static/).

**Note:** Pay attention if using the "static" examples, as the `manifest` subcommand should use `daemonset` and NOT `pod`.

### Set configuration details

`export VIP=192.168.0.40`

`export INTERFACE=<interface>`

### Configure to use a container runtime

The easiest method to generate a manifest is using the container itself, below will create an alias for different container runtimes.

#### containerd
`alias kube-vip="ctr run --rm --net-host docker.io/plndr/kube-vip:0.2.3 vip"`

#### Docker
`alias kube-vip="docker run --network host --rm plndr/kube-vip:0.2.3"`

### BGP Example

This configuration will create a manifest that will start `kube-vip` providing **controlplane** and **services** management. **Unlike** ARP, all nodes in the BGP configuration will advertise virtual IP addresses. 

**Note** we bind the address to `lo` as we don't want multiple devices that have the same address on public interfaces. We can specify all the peers in a comma seperate list in the format of `address:AS:password:multihop`.

**Note 2** we pass the `--inCluster` flag as this is running as a daemonSet within the Kubernetes cluster and therefore will have access to the token inside the running pod.

**Note 2** we pass the `--taint` flag as we're deploying `kube-vip` as both a daemonset and as advertising controlplane, we want to taint this daemonset to only run on the worker nodes.

`export INTERFACE=lo`

```
kube-vip manifest daemonset \
    --interface $INTERFACE \
    --vip $VIP \
    --controlplane \
    --services \
    --inCluster \
    --taint \
    --bgp \
    --bgppeers 192.168.0.10:65000::false,192.168.0.11:65000::false
```

### Generated Manifest

```
apiVersion: apps/v1
kind: DaemonSet
metadata:
  creationTimestamp: null
  name: kube-vip-ds
  namespace: kube-system
spec:
  selector:
    matchLabels:
      name: kube-vip-ds
  template:
    metadata:
      creationTimestamp: null
      labels:
        name: kube-vip-ds
    spec:
      containers:
      - args:
        - manager
        env:
        - name: vip_arp
          value: "false"
        - name: vip_interface
          value: lo
        - name: port
          value: "6443"
        - name: vip_cidr
          value: "32"
        - name: cp_enable
          value: "true"
        - name: cp_namespace
          value: kube-system
        - name: svc_enable
          value: "true"
        - name: bgp_enable
          value: "true"
        - name: bgp_peers
          value: "192.168.0.10:65000::false,192.168.0.11:65000::false"
        - name: vip_address
          value: 192.168.0.40
        image: plndr/kube-vip:0.2.3
        imagePullPolicy: Always
        name: kube-vip
        resources: {}
        securityContext:
          capabilities:
            add:
            - NET_ADMIN
            - SYS_TIME
      hostNetwork: true
      serviceAccountName: kube-vip
      nodeSelector:
        node-role.kubernetes.io/master: "true"
      tolerations:
      - effect: NoSchedule
        key: node-role.kubernetes.io/master
  updateStrategy: {}
```

### Manifest Overview

- `serviceAccountName: kube-vip` - this specifies the user in the `rbac` that will give us the permissions to get/update services.
- `hostNetwork: true` - This pod will need to modify interfaces (for VIPs)
- `env {...}` - We pass the configuration into the kube-vip pod through environment variables.