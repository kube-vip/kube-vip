# Kube-Vip as a DaemonSet

## DaemonSet

Some Kubernetes distributions can bring up a Kubernetes cluster without depending on a pre-existing VIP (but they may be configured to support one). A prime example of this would be K3s which can be configured to start and also sign the certificates to allow incoming traffic to a virtual IP. Given we don't need the VIP to exist before the cluster, we can bring up the K3s node(s) and then add `kube-vip` as a DaemonSet for all control plane nodes.

If the Kubernetes installer allows for adding a virtual IP as an additional [SAN](https://en.wikipedia.org/wiki/Subject_Alternative_Name) to the API server certificate, we can apply `kube-vip` to the cluster once the first node has been brought up.

Unlike running `kube-vip` as a [static Pod](/install_static) there are a few more things that may need configuring when running `kube-vip` as a DaemonSet. This page will cover primarily the differences.

## Kube-Vip as HA, Load Balancer, or both

The functionality of `kube-vip` depends on the flags used to create the static Pod manifest. By passing in `--controlplane` we instruct `kube-vip` to provide and advertise a virtual IP to be used by the control plane. By passing in `--services` we tell `kube-vip` to provide load balancing for Kubernetes Service resources created inside the cluster. With both enabled, `kube-vip` will manage a virtual IP address that is passed through its configuration for a highly available Kubernetes cluster. It will also watch Services of type `LoadBalancer` and once their `spec.LoadBalancerIP` is updated (typically by a cloud controller, including (optionally) the one provided by kube-vip in [on-prem](/usage/on-prem) scenarios) it will advertise this address using BGP/ARP. In this example, we will use both when generating the manifest.

## Create the RBAC settings

Since `kube-vip` as a DaemonSet runs as a regular resource instead of a static Pod, it still needs the correct access to be able to watch Kubernetes Services and other objects. In order to do this, RBAC resources must be created which include a ServiceAccount, ClusterRole, and ClusterRoleBinding and can be applied this with the command:

```
kubectl apply -f https://kube-vip.io/manifests/rbac.yaml
```

## Generating a Manifest

In order to create an easier experience of consuming the various functionality within `kube-vip`, we can use the `kube-vip` container itself to generate our DaemonSet manifest. We do this by running the `kube-vip` image as a container and passing in the various [flags](/flags/) for the capabilities we want to enable. Generating a `kube-vip` manifest for running as a DaemonSet is almost identical to the process when running `kube-vip` as a [static Pod](/install_static). Only a few flags are different between the two processes. Therefore, refer back to the [Generating a Manifest](/install_static/#generating-a-manifest) section on the [static Pod installation page](/install_static) for the main process steps.

### ARP Example for DaemonSet

When creating the `kube-vip` installation manifest as a DaemonSet, the `manifest` subcommand takes the value `daemonset` as opposed to the `pod` value. The flags `--inCluster` and `--taint` are also needed to configure the DaemonSet to use a ServiceAccount and affine the `kube-vip` Pods to control plane nodes thereby preventing them from running on worker instances.

```
kube-vip manifest daemonset \
    --interface $INTERFACE \
    --address $VIP \
    --inCluster \
    --taint \
    --controlplane \
    --services \
    --arp \
    --leaderElection
```

#### Example ARP Manifest

```yaml
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
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: node-role.kubernetes.io/master
                operator: Exists
            - matchExpressions:
              - key: node-role.kubernetes.io/control-plane
                operator: Exists
      containers:
      - args:
        - manager
        env:
        - name: vip_arp
          value: "true"
        - name: port
          value: "6443"
        - name: vip_interface
          value: ens160
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
        - name: address
          value: 192.168.0.40
        image: ghcr.io/kube-vip/kube-vip:v0.4.0
        imagePullPolicy: Always
        name: kube-vip
        resources: {}
        securityContext:
          capabilities:
            add:
            - NET_ADMIN
            - NET_RAW
            - SYS_TIME
      hostNetwork: true
      serviceAccountName: kube-vip
      tolerations:
      - effect: NoSchedule
        operator: Exists
      - effect: NoExecute
        operator: Exists
  updateStrategy: {}
status:
  currentNumberScheduled: 0
  desiredNumberScheduled: 0
  numberMisscheduled: 0
  numberReady: 0
```

### BGP Example for DaemonSet

This configuration will create a manifest that starts `kube-vip` providing control plane VIP and Kubernetes Service management. Unlike ARP, all nodes in the BGP configuration will advertise virtual IP addresses.

**Note** we bind the address to `lo` as we don't want multiple devices that have the same address on public interfaces. We can specify all the peers in a comma-separated list in the format of `address:AS:password:multihop`.

`export INTERFACE=lo`

```
kube-vip manifest daemonset \
    --interface $INTERFACE \
    --address $VIP \
    --inCluster \
    --taint \
    --controlplane \
    --services \
    --bgp \
    --localAS 65000 \
    --bgpRouterID 192.168.0.2 \
    --bgppeers 192.168.0.10:65000::false,192.168.0.11:65000::false

```

#### Example BGP Manifest

```yaml
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
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: node-role.kubernetes.io/master
                operator: Exists
            - matchExpressions:
              - key: node-role.kubernetes.io/control-plane
                operator: Exists
      containers:
      - args:
        - manager
        env:
        - name: vip_arp
          value: "false"
        - name: port
          value: "6443"
        - name: vip_interface
          value: ens160
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
        - name: address
          value: 192.168.0.40
        image: ghcr.io/kube-vip/kube-vip:v0.4.0
        imagePullPolicy: Always
        name: kube-vip
        resources: {}
        securityContext:
          capabilities:
            add:
            - NET_ADMIN
            - NET_RAW
            - SYS_TIME
      hostNetwork: true
      serviceAccountName: kube-vip
      tolerations:
      - effect: NoSchedule
        operator: Exists
      - effect: NoExecute
        operator: Exists
  updateStrategy: {}
status:
  currentNumberScheduled: 0
  desiredNumberScheduled: 0
  numberMisscheduled: 0
  numberReady: 0
```

#### Managing a `routerID` as a DaemonSet

The `routerID` needs to be unique on each node that participates in BGP advertisements. In order to do this, we can modify the manifest so that when `kube-vip` starts it will look up its local address and use that as the `routerID`. Add the following to the `env[]` array of the container:

```yaml
- name: bgp_routerinterface
  value: "ens160"
```

### DaemonSet Manifest Overview

Once the manifest for `kube-vip` as a DaemonSet is generated, these are some of the notable differences over the [static Pod](/install_static) manifest and their significance.

- `nodeSelector`: Ensures that DaemonSet Pods only run on control plane nodes.
- `serviceAccountName: kube-vip`: Specifies the ServiceAccount name that will be used to get/update Kubernetes Service resources.
- `tolerations`: Allows scheduling to control plane nodes that normally specify `NoSchedule` or `NoExecute` taints.
