# Usage

The below instructions *should just work* on Kubernetes regardless of architecture, Linux as the Operating System is the only requirement.

## The `tl;dr` guide

If you just want things to "work", then you can quickly install the "latest" components:

**Install the `plndr-cloud-provider`, `starboard` and `kube-vip`**

```
kubectl apply -f https://kube-vip.io/manifests/controller.yaml
kubectl apply -f https://kube-vip.io/manifests/kube-vip.yaml
```

**Create the `cidr` for the `default` namespace**

```
kubectl create configmap --namespace kube-system plndr --from-literal cidr-default=192.168.0.200/29
```

Creating services of `type: LoadBalancer` in the default namespace will now take addresses from the cidr defined in the `configmap`.

**Additional namespaces**

Edit the `configmap` and add in the cidr ranges for those namespaces, the key in the cidr should be `cidr-<namespace>`, then ensure that `kube-vip` is deployed into that namespace with the above `apply` command with the `-n namespace` flag.

## Deploying `kube-vip` in a Kubernetes cluster (in detail)

### Deploy the `plndr-cloud-provider`

From the GitHub repository [https://github.com/plunder-app/plndr-cloud-provider/tree/master/example/pod](https://github.com/plunder-app/plndr-cloud-provider/tree/master/example/pod), find the version of the plunder cloud provider manifest (although typically the highest version number will provider more functionality/stability). The [raw] option in Github will provide the url that can be applied directly with a `kubectl apply -f <url>`.

The following output should appear when the manifest is applied: 

```
serviceaccount/plunder-cloud-controller created
clusterrole.rbac.authorization.k8s.io/system:plunder-cloud-controller-role created
clusterrolebinding.rbac.authorization.k8s.io/system:plunder-cloud-controller-binding created
pod/plndr-cloud-provider created
```

We can validate the cloud-provider by examining the pods:

`kubectl logs -n kube-system plndr-cloud-provider -f`

#### The `plndr-cloud-provider` `configmap`

To manage the ranges for the load-balancer instances, the `plndr-cloud-provider` has a `configmap` held in the `kube-system` namespace. The structure for the key/values within the `configmap` should be that the key is in the format `cidr-<namespace>` and teh value should be the cidr range.

Example Configmap:

```
apiVersion: v1
kind: ConfigMap
metadata:
  name: plndr
  namespace: kube-system
data:
  cidr-default: 192.168.0.200/29
  cidr-plunder: 192.168.0.210/29
```

### Deploy `starboard`

This requires no customisation as it simply monitors the `configMap` managed by the plndr-cloud-provider.

From the GitHub repository [https://github.com/plunder-app/starboard/tree/master/examples/daemonset](https://github.com/plunder-app/starboard/tree/master/examples/daemonset) find the version of the starboard to deploy (although typically the highest version number will provider more functionality/stability). The [raw] option in Github will provide the url that can be applied directly with a `kubectl apply -f <url>`.

The following output should appear when the manifest is applied: 
```
serviceaccount/starboard created
role.rbac.authorization.k8s.io/starboard-role created
rolebinding.rbac.authorization.k8s.io/starboard-role-bind created
daemonset.apps/starboard-ds created
```

### Deploy `kube-vip`

From the GitHub repository [https://github.com/plunder-app/kube-vip/tree/master/example/deploy](https://github.com/plunder-app/kube-vip/tree/master/example/deploy) find the version of the kube-vip to deploy (although typically the highest version number will provider more functionality/stability). The [raw] option in Github will provide the url that can be applied directly with a `kubectl apply -f <url>`.

The following output should appear when the manifest is applied: 
```
serviceaccount/vip created
role.rbac.authorization.k8s.io/vip-role created
rolebinding.rbac.authorization.k8s.io/vip-role-bind created
deployment.apps/kube-vip-cluster created
```

*NOTE* The manifest for the `kube-vip` deployment has rules to ensure affinity (pods are always distributed to different nodes for HA). By default the replicas are set to `3` in the event you have less than `3` worker nodes then those replicas will sit as `pending`. This in itself isn't an issue, it means when new workers are added then they will be scheduled. *However*, tooling such as `kapps` will inspect the manifest before it's applied an error because of issues such as this.

#### Editing `kube-vip` configuration

Either download and edit the manifest locally or appy as above and edit the deployment with `kubectl edit deploy/kube-vip-cluster` (change namespace where appropriate `-n`)

```
        - name: vip_interface
          value: ens192
        - name: vip_configmap
          value: plndr
        - name: vip_arp
          value: "true"
        - name: vip_loglevel
          value: "5"
```

- `vip_interface` - defines the interface that the VIP will bind to
- `vip_configmap` - defines the configmap that `kube-vip` will watch for service configuration
- `vip_arp` - determines if ARP broadcasts are enabled
- `vip_loglevel` - determines the verbosity of logging

## Testing

This testing is performed on a brand new three node (`1.17.0`, 1 **Master**, 2 **Worker**) cluster, running Calico as the CNI plugin. 

#### Deploy NGINX

```
kubectl apply -f https://k8s.io/examples/application/deployment.yaml
```

#### Create Load-Balancer

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx-loadbalancer
```

#### View Load-Balancer

```
$ k describe service nginx-loadbalancer
Name:                     nginx-loadbalancer
Namespace:                default
Labels:                   <none>
Annotations:              <none>
Selector:                 app=nginx
Type:                     LoadBalancer
IP:                       10.96.184.221
LoadBalancer Ingress:     192.168.0.81
Port:                     <unset>  80/TCP
TargetPort:               80/TCP
NodePort:                 <unset>  31138/TCP
Endpoints:                172.16.198.194:80,172.16.232.2:80
Session Affinity:         None
External Traffic Policy:  Cluster
Events:
  Type    Reason                Age   From                Message
  ----    ------                ----  ----                -------
  Normal  EnsuringLoadBalancer  30s   service-controller  Ensuring load balancer
  Normal  EnsuredLoadBalancer   29s   service-controller  Ensured load balancer
```

Any host with on the same network or has the correct routes configured should now be able to use the load-balancer address displayed in the configuration: `LoadBalancer Ingress:     192.168.0.81`

## Using other namespaces

In this example we'll deploy and load-balance within the namespace `plunder`

### Create the namespace

`kubectl create namespace plunder`

### Add a network range/cidr for this namespace

`kubectl edit -n kube-system configmap/plndr`

We will add the range 192.168.0.210/29 for the namespace plunder underneath the existing range for the namespace default:

```
apiVersion: v1
data:
  cidr-default: 192.168.0.200/29
  cidr-plunder: 192.168.0.210/29
<...>
```

### Deploy `kube-vip` in the namespace **plunder**

In the same way we deployed `kube-vip` into the default namespace we can deploy the same manifest into a different namespace using `-n namespace` e.g.

**Note** change the version of manifest when actually deploying!

```
kubectl apply -f https://github.com/plunder-app/kube-vip/raw/master/example/deploy/0.1.3.yaml -n plunder
```

### Deploy nginx

```
kubectl create deployment --image nginx plunder-nginx --namespace plunder
```

### Create a load balancer

```
kubectl expose deployment plunder-nginx --port=80 --type=LoadBalancer --namespace plunder
```

## Troubleshooting

Typically the logs from the `kube-vip` controller will reveal the most clues as to where a problem may lie.

The `ClusterRoleBinding` is missing will result in the following:

```
E0229 17:36:38.014351       1 retrywatcher.go:129] Watch failed: unknown (get endpoints)
E0229 17:36:38.014352       1 retrywatcher.go:129] Watch failed: unknown (get endpoints)
```

If Load-balancing isn't working as expected, then examine the `iptables` on the node running `kube-vip`:

```
sudo iptables -L -n -t nat | more
Chain PREROUTING (policy ACCEPT)
target     prot opt source               destination         
cali-PREROUTING  all  --  0.0.0.0/0            0.0.0.0/0            /* cali:6gwbT8clXdHdC1b1 */
ACCEPT     all  --  0.0.0.0/0            192.168.0.80/29     
KUBE-SERVICES  all  --  0.0.0.0/0            0.0.0.0/0            /* kubernetes service portals */
DOCKER     all  --  0.0.0.0/0            0.0.0.0/0            ADDRTYPE match dst-type LOCAL
```

Ensure that the `cidr` is present in the PREROUTING table, if this is missing ensure that the starboard daemonset is running.
