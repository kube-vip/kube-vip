# K3s Overview

`kube-vip` works on [K3s environments](https://k3s.io/) similar to most others with the exception of how it gets deployed. Because K3s is able to bootstrap a single server (control plane node) without the availability of the load balancer fronting it, `kube-vip` can be installed as a DaemonSet.

## Prerequisites (on Equinix Metal)

In order to make ARP work on Equinix Metal, follow the [metal-gateway](https://metal.equinix.com/developers/docs/networking/metal-gateway/) guide to have public VLAN subnet which can be used for the load balancer IP.

## Clean Environment

This step is optional but recommended if a K3s installation previously existed.

```
rm -rf /var/lib/rancher /etc/rancher ~/.kube/*; \
ip addr flush dev lo; \
ip addr add 127.0.0.1/8 dev lo;
```

## Step 1: Create Manifests Folder

K3s has an optional manifests directory that will be searched to [auto-deploy](https://rancher.com/docs/k3s/latest/en/advanced/#auto-deploying-manifests) any manifests found within. Create this directory first in order to later place the `kube-vip` resources inside.

```
mkdir -p /var/lib/rancher/k3s/server/manifests/
```

## Step 2: Upload Kube-Vip RBAC Manifest

As `kube-vip` runs as a DaemonSet under K3s and not a static Pod, we will need to ensure that the required permissions exist for it to communicate with the API server. RBAC resources are needed to ensure a ServiceAccount exists with those permissions and bound appropriately.

Get the RBAC manifest and place in the auto-deploy directory:

```
curl https://kube-vip.io/manifests/rbac.yaml > /var/lib/rancher/k3s/server/manifests/kube-vip-rbac.yaml
```

## Step 3: Generate a Kube-Vip DaemonSet Manifest

Refer to the [DaemonSet manifest generation documentation](/docs/install_daemonset/index.md#generating-a-manifest) for the process to complete this step.

Either store this generated manifest separately in the `/var/lib/rancher/k3s/server/manifests/` directory, or append to the existing RBAC manifest called `kube-vip-rbac.yaml`. As a general best practice, it is a cleaner approach to place all related resources into a single YAML file.

> Note: Remember to include YAML document delimiters (`---`) when composing multiple documents.

## Step 4: Install a HA K3s Cluster

There are multiple ways to install K3s including `[k3sup](https://k3sup.dev/)` or [running the binary](https://rancher.com/docs/k3s/latest/en/quick-start/) locally. Whichever method you choose, the `--tls-san` flag must be passed with the same IP when generating the `kube-vip` DaemonSet manifest when installing the first server (control plane) instance. This is so that K3s generates an API server certificate with the `kube-vip` virtual IP address.

Once the cluster is installed, you should be able to edit the `kubeconfig` file generated from the process and use the `kube-vip` VIP address to access the control plane.

## Step 5: Service Load Balancing

If wanting to use the `kube-vip` [cloud controller](/docs/usage/cloud-provider/), pass the `--disable servicelb` flag so K3s will not attempt to render Kubernetes Service resources of type `LoadBalancer`. If building with `k3sup`, the flag should be given as an argument to the `--k3s-extra-args` flag itself: `--k3s-extra-args "--disable servicelb"`. To install the `kube-vip` cloud controller, follow the additional steps in the [cloud controller guide](/docs/usage/cloud-provider/#install-the-kube-vip-cloud-provider).
