# Load Balancing a Kubernetes Cluster (Control-Plane)

This document covers the newer (post `0.1.5`) method for using `kube-vip` to provide HA for a Kubernetes Cluster. The documentation for older releases can be found [here](./0.1.4/)

This document covers all of the details for using `kube-vip` to build a HA Kubernetes cluster

`tl;dr version`
- Generate/modify first node `kube-vip` config/manifest
- `init` first node
- `join` remaining nodes
- Add remaining config/manifests

Below are examples of the steps required:

```
# First Node
sudo docker run --network host --rm plndr/kube-vip:0.1.5 kubeadm init --interface ens192 --vip 192.168.0.81 --startAsLeader=true | sudo tee /etc/kubernetes/manifests/vip.yaml

sudo kubeadm init --kubernetes-version 1.17.0 --control-plane-endpoint 192.168.0.81 --upload-certs

# Additional Node(s)

sudo kubeadm join 192.168.0.81:6443 --token w5atsr.blahblahblah --control-plane --certificate-key abc123

sudo docker run -v /etc/kubernetes/admin.conf:/etc/kubernetes/admin.conf --network host --rm plndr/kube-vip:0.1.5 kubeadm join --interface ens192 --vip 192.168.0.81 --startAsLeader=false | sudo tee /etc/kubernetes/manifests/vip.yaml
```


## Infrastructure architecture

The infrastructure for our example HA Kubernetes cluster is as follows:

| Node           | Address    |
|----------------|------------|
| VIP            | 10.0.0.75 |
| controlPlane01 | 10.0.0.70 |
| controlPlane02 | 10.0.0.71 |
| controlPlane03 | 10.0.0.72 |

All nodes are running Ubuntu 18.04, Docker CE and will use Kubernetes 1.17.0.

### Generate the `kube-vip` configuration

Kube-Vip no longer requires storing it's configuration in a seperate directory and will now store its configuration in the actual manifest that defines the static pods. 

```
sudo docker run --network host \
	--rm plndr/kube-vip:0.1.5 \
	kubeadm init \
	--interface ens192 \
	--vip 192.168.0.75 \
	--startAsLeader=true | sudo tee /etc/kubernetes/manifests/vip.yaml
```

The above command will "initialise" the manifest within the `/etc/kubernetes/manifests` directory, that will be started when we actually initialise our Kubernetes cluster with `kubeadm init`

### Modify the configuration

**Cluster Configuration**
As this node will be the first node, it will need to elect itself leader as until this occurs the VIP won’t be activated!

`--startAsLeader=true`

**VIP Config**
We will need to set our VIP address to `192.168.0.75` with `--vip 192.168.0.75` and to ensure all hosts are updated when the VIP moves we will enable ARP broadcasts `--arp` (defaults to `true`)

**Load Balancer**
We will configure the load balancer to sit on the standard API-Server port `6443` and we will configure the backends to point to the API-servers that will be configured to run on port `6444`. Also for the Kubernetes Control Plane we will configure the load balancer to be of `type: tcp`.

We can also use `6443` for both the VIP and the API-Servers, in order to do this we need to specify that the api-server is bound to it's local IP. To do this we use the `--apiserver-advertise-address` flag as part of the `init`, this means that we can then bind the same port to the VIP and we wont have a port conflict.

**vip.yaml** Static-pod Manifest

`$ sudo cat /etc/kubernetes/manifests/vip.yaml`


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
    - start
    env:
    - name: vip_arp
      value: "true"
    - name: vip_interface
      value: ens192
    - name: vip_address
      value: 192.168.0.81
    - name: vip_startleader
      value: "true"
    - name: vip_addpeerstolb
      value: "true"
    - name: vip_localpeer
      value: controlPlane01:192.168.0.70:10000
    - name: lb_backendport
      value: "6443"
    - name: lb_name
      value: Kubeadm Load Balancer
    - name: lb_type
      value: tcp
    - name: lb_bindtovip
      value: "true"
    image: plndr/kube-vip:0.1.5
    imagePullPolicy: Always
    name: kube-vip
    resources: {}
    securityContext:
      capabilities:
        add:
        - NET_ADMIN
        - SYS_TIME
  hostNetwork: true
status: {}
```

### First Node

To generate the basic Kubernetes static pod `yaml` configuration:

Make sure that the manifest directory exists: `sudo mkdir -p /etc/kubernetes/manifests/`

```
sudo docker run --network host \
	--rm plndr/kube-vip:0.1.5 \
	kubeadm init \
	--interface ens192 \
	--vip 192.168.0.75 \
	--startAsLeader=true | sudo tee /etc/kubernetes/manifests/vip.yaml
```

Ensure that `image: plndr/kube-vip:<x>` is modified to point to a specific version (`0.1.5` at the time of writing), refer to [docker hub](https://hub.docker.com/r/plndr/kube-vip/tags) for details. 

The **vip** is set to `192.168.0.75` and this first node will elect itself as leader, and as part of the `kubeadm init` it will use the VIP in order to speak back to the initialising api-server.

`sudo kubeadm init --control-plane-endpoint “192.168.0.75:6443” --apiserver-bind-port 6444 --upload-certs --kubernetes-version “v1.17.0”`

Once this node is up and running we will be able to see the control-plane pods, including the `kube-vip` pod:

```
$ kubectl get pods -A
NAMESPACE     NAME                                     READY   STATUS    RESTARTS   AGE
<...>
kube-system   kube-vip-controlplane01                  1/1     Running   0          10m
```

### Remaining Nodes


At this point **DON’T** generate the manifests, this is due to some bizarre `kubeadm/kubelet` behaviour.

```
  kubeadm join 192.168.0.75:6443 --token <tkn> \
    --discovery-token-ca-cert-hash sha256:<hash> \
    --control-plane --certificate-key <key> 

```

**After** this node has been added to the cluster, we can add the manifest to also add this node as a `kube-vip` member. (Adding the manifest afterwards doesn’t interfere with `kubeadm`). 

```
sudo docker run \
	-v /etc/kubernetes/admin.conf:/etc/kubernetes/admin.conf \
	--network host \
	--rm plndr/kube-vip:0.1.5 \
	kubeadm join \
	--interface ens192 \
	--vip 192.168.0.81 \
	--startAsLeader=false | sudo tee /etc/kubernetes/manifests/vip.yaml

```

Once this node is added we will be able to see that the `kube-vip` pod is up and running as expected:

```
user@controlPlane01:~$ kubectl get pods -A | grep vip
kube-system   kube-vip-controlplane01                  1/1     Running             1          16m
kube-system   kube-vip-controlplane02                  1/1     Running             0          18m
kube-system   kube-vip-controlplane03                  1/1     Running             0          20m

```

If we look at the logs, we can see that the VIP is running on the second node and we’re waiting for our third node to join the cluster:

```
$ kubectl logs kube-vip-controlplane02  -n kube-system
time=“2020-02-12T15:33:09Z” level=info msg=“The Node [192.168.0.70:10000] is leading”
time=“2020-02-12T15:33:09Z” level=info msg=“The Node [192.168.0.70:10000] is leading”

```