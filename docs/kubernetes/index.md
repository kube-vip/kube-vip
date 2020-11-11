# Usage

The below instructions *should just work* on Kubernetes regardless of architecture, Linux as the Operating System is the only requirement.

## The `tl;dr` guide

If you just want things to "work", then you can quickly install the "latest" components:

**NOTE** the `kube-vip.yaml` may need customising to set ARP/BGP OR to configure which interface to bind VIPs too.

**Install the `plndr-cloud-provider`, and `kube-vip`**

```
kubectl apply -f https://kube-vip.io/manifests/controller.yaml
kubectl apply -f https://kube-vip.io/manifests/kube-vip.yaml
```

**Create the `cidr` for the `global` namespace**

```
kubectl create configmap --namespace kube-system plndr --from-literal cidr-global=192.168.0.200/29
```

Creating services of `type: LoadBalancer` in the default namespace will now take addresses from the **global** cidr defined in the `configmap`.

**Additional namespaces**

Edit the `configmap` and add in the cidr ranges for those namespaces, the key in the cidr should be `cidr-<namespace>`, then ensure that `kube-vip` is deployed into that namespace with the above `apply` command with the `-n namespace` flag.

## The Detailed guide

### Deploy the `plndr-cloud-provider`

To deploy the [latest] then `kubectl apply -f https://kube-vip.io/manifests/controller.yaml`, specific versions should be found in the repository as detailed below:

From the GitHub repository [https://github.com/plunder-app/plndr-cloud-provider/tree/master/example/pod](https://github.com/plunder-app/plndr-cloud-provider/tree/master/example/pod), find the version of the plunder cloud provider manifest (although typically the highest version number will provider more functionality/stability). The [raw] option in Github will provide the url that can be applied directly with a `kubectl apply -f <url>`.

The following output should appear when the manifest is applied: 

```
serviceaccount/plunder-cloud-controller created
clusterrole.rbac.authorization.k8s.io/system:plunder-cloud-controller-role created
clusterrolebinding.rbac.authorization.k8s.io/system:plunder-cloud-controller-binding created
pod/plndr-cloud-provider created
```

We can validate the cloud-provider by examining the pods:

`kubectl logs -n kube-system plndr-cloud-provider-0 -f`

#### The `plndr-cloud-provider` `configmap`

The `configmap` details a CIDR range *per* namespace, however as of (`kube-vip 0.2.1` and `plnder-cloud-provider 0.1.4`), there is now the option of having a **global** CIDR range (`cidr-global)`.  

To manage the ranges for the load-balancer instances, the `plndr-cloud-provider` has a `configmap` held in the `kube-system` namespace. The structure for the key/values within the `configmap` should be that the key is in the format `cidr-<namespace>` and the value should be the cidr range.

Example Configmap:

```
apiVersion: v1
kind: ConfigMap
metadata:
  name: plndr
  namespace: kube-system
data:
  cidr-default: 192.168.0.200/29
  cidr-global: 192.168.0.210/29
```

### Deploying `kube-vip`

To use `kube-vip` in Layer2/ARP the follow this [guide](/arp/)

To use `kube-vip` in Layer3/BGP the follow this [guide](/bgp/)

