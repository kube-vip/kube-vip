# K3s overview (on Equinix Metal)

## Prerequisites

In order to make ARP work on Equinix Metal, you need to follow [metal-gateway](https://metal.equinix.com/developers/docs/networking/metal-gateway/) guide to have public VLAN subnet, which you can use as your loadbalancer IP.

## Optional Tidy environment (best if something was running before)
```
rm -rf /var/lib/rancher /etc/rancher ~/.kube/*; \ 
ip addr flush dev lo; \
ip addr add 127.0.0.1/8 dev lo; 
```

## Step 1: Create Manifests folder

This is required, this folder will contain all of the generated manifests that `k3s` will execute as it starts. We will create it before `k3s` and place our `kube-vip` manifests within it.

```
mkdir -p /var/lib/rancher/k3s/server/manifests/
```

## Step 2: Get rbac for `Kube-Vip`

As `kube-vip` runs inside of the Kubernetes cluster, we will need to ensure that the required permissions exist.

```
curl https://kube-vip.io/manifests/rbac.yaml > /var/lib/rancher/k3s/server/manifests/rbac.yaml
```

## Step 3: Generate kube-vip (A VIP address for the network will be required)

Configure your virtual IP (for the control plane) and interface that will expose this VIP first.

```
export VIP=x.x.x.x
export INTERFACE=bind0 # or ethX depends on your networking setup
```

Modify the `VIP` and `INTERFACE` to match the floating IP address you'd like to use and the interface it should bind to.

To generate the manifest we have two options! We can generate the manifest from [kube-vip.io](kube-vip.io) or use a kube-vip image to generate the manifest!

## Step 3.1: Generate from kube-vip.io
 
```
curl -sL kube-vip.io/k3s | vipAddress=$VIP vipInterface=$INTERFACE sh | sudo tee /var/lib/rancher/k3s/server/manifests/vip.yaml
```

## Step 3.2 Generate from container image


### Get latest version

 We can parse the GitHub API to find the latest version (or we can set this manually)

`KVVERSION=$(curl -sL https://api.github.com/repos/kube-vip/kube-vip/releases | jq -r ".[0].name")`

or manually:

`export KVVERSION=vx.x.x`

The easiest method to generate a manifest is using the container itself, below will create an alias for different container runtimes.

### containerd
`alias kube-vip="ctr run --rm --net-host ghcr.io/kube-vip/kube-vip:$KVVERSION vip /kube-vip"`

### Docker
`alias kube-vip="docker run --network host --rm ghcr.io/kube-vip/kube-vip:KVVERSION"`


```
kube-vip manifest daemonset \
  --interface $INTERFACE \
  --vip $VIP \
  --controlplane \
  --services \
  --inCluster \
  --taint \
  --arp 
```

## Step 4: Up Cluster

From online `-->`

```
curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--write-kubeconfig-mode 644 \
-t agent-secret --tls-san $VIP" sh -
```

From local `-->`

```
sudo ./k3s server --tls-san $VIP
```

## Step 5: Service Load-Balancing

For this refer to the [on-prem](../on-prem) documentation 
