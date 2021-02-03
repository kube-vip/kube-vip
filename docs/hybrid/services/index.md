# Kube-vip services 

We've designed `kube-vip` to be as de-coupled or agnostic from other components that may exist within a Kubernetes cluster as possible. This has lead to `kube-vip` having a very simplistic but robust approach to advertising Kubernetes services to the outside world and marking these services as ready to use.

## Flow

This section details the flow of events in order for `kube-vip` to advertise a Kubernetes service:

1. An end user exposes a application through Kubernetes as a LoadBalancer => `kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx`
2. Within the Kubernetes cluster a service object is created with the `svc.Spec.Type = ServiceTypeLoadBalancer`
3. A controller (typically a Cloud Controller) has a loop that "watches" for services of the type `LoadBalancer`.
4. The controller now has the responsibility of providing an IP address for this service along with doing anything that is network specific for the environment where the cluster is running.
5. Once the controller has an IP address it will update the service `svc.Spec.LoadBalancerIP` with it's new IP address.
6. The `kube-vip` pods also implement a "watcher" for services that have a `svc.Spec.LoadBalancerIP` address attached.
7. When a new service appears `kube-vip` will start advertising this address to the wider network (through BGP/ARP) which will allow traffic to come into the cluster and hit the service network.
8. Finally `kube-vip` will update the service status so that the API reflects that this LoadBalancer is ready. This is done by updating the `svc.Status.LoadBalancer.Ingress` with the VIP address.

## CCM

We can see from the [flow](#Flow) above that `kube-vip` isn't coupled to anything other than the Kubernetes API, and will only act upon an existing Kubernetes primative (in this case the object of type `Service`). This makes it easy for exist CCMs to simply apply their logic to services of type LoadBalancer and leave `kube-vip` to take the next steps to advertise these load-balancers to the outside world. 

The below instructions *should just work* on Kubernetes regardless of architecture, Linux as the Operating System is the only requirement.

## Using the Plunder Cloud Provider (CCM)

If you just want things to "work", then you can quickly install the "latest" components:

**Install the `plndr-cloud-provider`

```
kubectl apply -f https://kube-vip.io/manifests/controller.yaml
```

**Create the `cidr` for the `global` namespace**

```
kubectl create configmap --namespace kube-system plndr --from-literal cidr-global=192.168.0.200/29
```

Creating services of `type: LoadBalancer` in the default namespace will now take addresses from the **global** cidr defined in the `configmap`.

**Additional namespaces**

Edit the `configmap` and add in the cidr ranges for those namespaces, the key in the cidr should be `cidr-<namespace>`, then ensure that `kube-vip` is deployed into that namespac
e with the above `apply` command with the `-n namespace` flag.

## The Detailed guide

### Deploy the `plndr-cloud-provider`

To deploy the [latest] then `kubectl apply -f https://kube-vip.io/manifests/controller.yaml`, specific versions should be found in the repository as detailed below:

From the GitHub repository [https://github.com/plunder-app/plndr-cloud-provider/tree/master/example/pod](https://github.com/plunder-app/plndr-cloud-provider/tree/master/example/p
od), find the version of the plunder cloud provider manifest (although typically the highest version number will provider more functionality/stability). The [raw] option in Githu
b will provide the url that can be applied directly with a `kubectl apply -f <url>`.

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

The `configmap` details a CIDR range *per* namespace, however as of (`kube-vip 0.2.1` and `plnder-cloud-provider 0.1.4`), there is now the option of having a **global** CIDR rang
e (`cidr-global)`.  

To manage the ranges for the load-balancer instances, the `plndr-cloud-provider` has a `configmap` held in the `kube-system` namespace. The structure for the key/values within th
e `configmap` should be that the key is in the format `cidr-<namespace>` and the value should be the cidr range.

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

### Expose a service

We can now expose a service and once the cloud provider has provided an address `kube-vip` will start to advertise that address to the outside world as shown below!

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx
```

We can also expose a specific address by specifying it on the command line:

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx --load-balancer-ip=1.1.1.1
```

### Using DHCP for Load Balancers (experimental)

With the latest release of `kube-vip` > 0.2.1, it is possible to use the local network DHCP server to provide `kube-vip` with a load-balancer address that can be used to access a
 Kubernetes service on the network. 

In order to do this we need to signify to `kube-vip` and the cloud-provider that we don't need one of their managed addresses. We do this by explicitly exposing a service on the 
address `0.0.0.0`. When `kube-vip` sees a service on this address it will create a `macvlan` interface on the host and request a DHCP address, once this address is provided it wi
ll assign it as the VIP and update the Kubernetes service!

```
$ k expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx-dhcp --load-balancer-ip=0.0.0.0; k get svc
service/nginx-dhcp exposed
NAME         TYPE           CLUSTER-IP      EXTERNAL-IP     PORT(S)        AGE
kubernetes   ClusterIP      10.96.0.1       <none>          443/TCP        17m
nginx-dhcp   LoadBalancer   10.97.150.208   0.0.0.0         80:31184/TCP   0s

{ ... a second or so later ... }

$ k get svc
NAME         TYPE           CLUSTER-IP      EXTERNAL-IP     PORT(S)        AGE
kubernetes   ClusterIP      10.96.0.1       <none>          443/TCP        17m
nginx-dhcp   LoadBalancer   10.97.150.208   192.168.0.155   80:31184/TCP   3s
```

### Using UPNP to expose a service to the outside world

With the latest release of `kube-vip` > 0.2.1, it is possible to expose a load-balancer on a specific port and using UPNP (on a supported gateway) expose this service to the inte
rnet.

Most simple networks look something like the following:

`<----- <internal network 192.168.0.0/24> <Gateway / router> <external network address> ----> Internet`

Using UPNP we can create a matching port on the `<external network address>` allowing your service to be exposed to the internet.

#### Enable UPNP

Add the following to the `kube-vip` `env:` section, and the rest should be completely automated. 

**Note** some environments may require (Unifi) will require `Secure mode` being `disabled` (this allows a host with a different address to register a port)

```
- name: enableUPNP
  value: "true"
```

#### Exposing a service

To expose a port successfully we'll need to change the command slightly:

`--target-port=80` the port of the application in the pods (HTT/NGINX)
`--port=32380` the port the service will be exposed on (and what you should connect to in order to receive traffic from the service)

`kubectl expose deployment plunder-nginx --port=32380 --target-port=80 --type=LoadBalancer --namespace plunder`

The above example should expose a port on your external (internet facing address), that can be tested externally with:

```
$ curl externalIP:32380
<!DOCTYPE html>
<html>
...
```

### Expose with Equinix Metal (using the `plndr-cloud-provider`)

Either through the CLI or through the UI, create a public IPv4 EIP address.. and this is the address you can expose through BGP!

```
# packet ip request -p xxx-bbb-ccc -f ams1 -q 1 -t public_ipv4                                                                   
+-------+---------------+--------+----------------------+
|   ID  |    ADDRESS    | PUBLIC |       CREATED        |
+-------+---------------+--------+----------------------+
| xxxxx |     1.1.1.1   | true   | 2020-11-10T15:57:39Z |
+-------+---------------+--------+----------------------+

kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx --load-balancer-ip=1.1.1.1
```

## Equinix Metal Overview (using the [Equinix Metal CCM](https://github.com/packethost/packet-ccm))

Below are two examples for running `type:LoadBalancer` services on worker nodes only and will create a daemonset that will run `kube-vip`. 

**NOTE** This use-case requires the [Equinix Metal CCM](https://github.com/packethost/packet-ccm) to be installed and that the cluster/kubelet is configured to use an "external" cloud provider.

### Using Annotations

This is important as the CCM will apply the BGP configuration to the [node annotations](https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations/) making it easy for `kube-vip` to find the networking configuration it needs to expose load balancer addresses. The `--annotations metal.equinix.com` will cause kube-vip to "watch" the annotations of the worker node that it is running on, once all of the configuarion has been applied by the CCM then the `kube-vip` pod is ready to advertise BGP addresses for the service.

```
kube-vip manifest daemonset \
  --interface $INTERFACE \
  --services \
  --bgp \
  --annotations metal.equinix.com \
  --inCluster | k apply -f -
```

### Using the existing CCM secret 

Alternatively it is possible to create a daemonset that will use the existing CCM secret to do an API lookup, this will allow for discovering the networking configuration needed to advertise loadbalancer addresses through BGP.

```
kube-vip manifest daemonset --interface $INTERFACE \
--services \
--inCluster \
--bgp \
--metal \
--provider-config /etc/cloud-sa/cloud-sa.json | kubectl apply -f -
```
