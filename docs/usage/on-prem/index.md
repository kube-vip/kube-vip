# Kube-vip on-prem 

We've designed `kube-vip` to be as de-coupled or agnostic from other components that may exist within a Kubernetes cluster as possible. This has lead to `kube-vip` having a very simplistic but robust approach to advertising Kubernetes services to the outside world and marking these services as ready to use.

## Flow

This section details the flow of events in order for `kube-vip` to advertise a Kubernetes service:

1. An end user exposes a application through Kubernetes as a LoadBalancer => `kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx`
2. Within the Kubernetes cluster a service object is created with the `svc.Spec.Type = LoadBalancer`
3. A controller (typically a Cloud Controller) has a loop that "watches" for services of the type `LoadBalancer`.
4. The controller now has the responsibility of providing an IP address for this service along with doing anything that is network specific for the environment where the cluster is running.
5. Once the controller has an IP address it will update the service `svc.Spec.LoadBalancerIP` with it's new IP address.
6. The `kube-vip` pods also implement a "watcher" for services that have a `svc.Spec.LoadBalancerIP` address attached.
7. When a new service appears `kube-vip` will start advertising this address to the wider network (through BGP/ARP) which will allow traffic to come into the cluster and hit the service network.
8. Finally `kube-vip` will update the service status so that the API reflects that this LoadBalancer is ready. This is done by updating the `svc.Status.LoadBalancer.Ingress` with the VIP address.

## CCM

We can see from the [flow](#Flow) above that `kube-vip` isn't coupled to anything other than the Kubernetes API, and will only act upon an existing Kubernetes primative (in this case the object of type `Service`). This makes it easy for existing CCMs to simply apply their logic to services of type LoadBalancer and leave `kube-vip` to take the next steps to advertise these load-balancers to the outside world. 


## Using the Kube-vip Cloud Provider

The below instructions *should just work* on Kubernetes regardless of architecture (Linux Operating System is the only requirement) - you can quickly install the "latest" components:

**Install the `kube-vip-cloud-provider`**

```
$ kubectl apply -f https://raw.githubusercontent.com/kube-vip/kube-vip-cloud-provider/main/manifest/kube-vip-cloud-controller.yaml
```

It uses a `statefulSet` and can always be viewed with the following command:

```
kubectl describe pods -n kube-system kube-vip-cloud-provider-0
```

**Create a global CIDR or IP Range**

Any `service` in any `namespace` can use an address from the global CIDR `cidr-global` or range `range-global`

```
kubectl create configmap --namespace kube-system kubevip --from-literal cidr-global=192.168.0.220/29
```
or
```
kubectl create configmap --namespace kube-system kubevip --from-literal range-global=192.168.1.220-192.168.1.230
```

Creating services of `type: LoadBalancer` in *any namespace* will now take addresses from the **global** cidr defined in the `configmap` unless a specific 


## The Detailed guide

### Deploy the Kube-vip Cloud Provider

**Install the `kube-vip-cloud-provider`**

```
$ kubectl apply -f https://raw.githubusercontent.com/kube-vip/kube-vip-cloud-provider/main/manifest/kube-vip-cloud-controller.yaml
```

The following output should appear when the manifest is applied: 

```
serviceaccount/kube-vip-cloud-controller created
clusterrole.rbac.authorization.k8s.io/system:kube-vip-cloud-controller-role created
clusterrolebinding.rbac.authorization.k8s.io/system:kube-vip-cloud-controller-binding created
statefulset.apps/kube-vip-cloud-provider created
```

We can validate the cloud provider by examining the pods and following the logs:

```
kubectl describe pods -n kube-system kube-vip-cloud-provider-0
kubectl logs -n kube-system kube-vip-cloud-provider-0 -f
```

### The Kube-vip Cloud Provider `configmap`

To manage the IP address ranges for the load balancer instances the `kube-vip-cloud-provider` uses a `configmap` held in the `kube-system` namespace. IP address ranges can be configured using:
- IP address pools by CIDR
- IP ranges [start address - end address]
- Multiple pools by CIDR per namespace
- Multiple IP ranges per namespace (handles overlapping ranges)
- Setting of static addresses through --load-balancer-ip=x.x.x.x

To control which IP address range is used for which service the following rules are applied:
- Global address pools (`cidr-global` or `range-global`) are available for use by *any* `service` in *any* `namespace`
- Namespace specific address pools (`cidr-<namespace>` or `range-<namespace>`) are *only* available for use by `service` in the *specific* `namespace`
- Static IP addresses can be applied to a load balancer `service` using the `loadbalancerIP` setting, even outside of the assigned ranges

Example Configmap:

```
$ kubectl get configmap -n kube-system kubevip -o yaml

apiVersion: v1
kind: ConfigMap
metadata:
  name: kubevip
  namespace: kube-system
data:
  cidr-default: 192.168.0.200/29                      # CIDR-based IP range for use in the default namespace
  range-development: 192.168.0.210-192.168.0.219      # Range-based IP range for use in the development namespace
  cidr-finance: 192.168.0.220/29,192.168.0.230/29     # Multiple CIDR-based ranges for use in the finance namespace
  cidr-global: 192.168.0.240/29                       # CIDR-based range which can be used in any namespace
```

### Expose a service

We can now expose a service and once the cloud provider has provided an address `kube-vip` will start to advertise that address to the outside world as shown below!

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx
```

or via a `service` YAML definition

```
apiVersion: v1
kind: Service
metadata:
  name: nginx
spec:
  ports:
  - name: http
    port: 80
    protocol: TCP
  selector:
    app: nginx
  type: LoadBalancer
  ```


We can also expose a specific address by specifying it on the command line:

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx --load-balancer-ip=1.1.1.1
```

or including it in the `service` definition:

```
apiVersion: v1
kind: Service
metadata:
  name: nginx
spec:
  ports:
  - name: http
    port: 80
    protocol: TCP
  selector:
    app: nginx
  type: LoadBalancer
  loadBalancerIP: "1.1.1.1"
```

### Using DHCP for Load Balancers (experimental)

With the latest release of `kube-vip` > 0.2.1, it is possible to use the local network DHCP server to provide `kube-vip` with a load-balancer address that can be used to access a
 Kubernetes service on the network. 

In order to do this we need to signify to `kube-vip` and the cloud-provider that we don't need one of their managed addresses. We do this by explicitly exposing a service on the 
address `0.0.0.0`. When `kube-vip` sees a service on this address it will create a `macvlan` interface on the host and request a DHCP address, once this address is provided it will assign it as the VIP and update the Kubernetes service!

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
