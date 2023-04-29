# Kube-Vip On-Prem

We've designed `kube-vip` to be as decoupled or agnostic from other components that may exist within a Kubernetes cluster as possible. This has lead to `kube-vip` having a very simplistic but robust approach to advertising Kubernetes Services to the outside world and marking these Services as ready to use.

## Cloud Controller Manager

`kube-vip` isn't coupled to anything other than the Kubernetes API and will only act upon an existing Kubernetes primitive (in this case the object of type `Service`). This makes it easy for existing [cloud controller managers (CCMs)](https://kubernetes.io/docs/concepts/architecture/cloud-controller/) to simply apply their logic to services of type LoadBalancer and leave `kube-vip` to take the next steps to advertise these load balancers to the outside world.

## Using the Kube-Vip Cloud Provider

The `kube-vip` cloud provider can be used to populate an IP address for Services of type `LoadBalancer` similar to what public cloud providers allow through a Kubernetes CCM. The below instructions *should just work* on Kubernetes regardless of the architecture (a Linux OS being the only requirement) and will install the latest components.

## Install the Kube-Vip Cloud Provider

The `kube-vip` cloud provider can be installed from the latest release in the `main` branch by using the following command:

```
kubectl apply -f https://raw.githubusercontent.com/kube-vip/kube-vip-cloud-provider/main/manifest/kube-vip-cloud-controller.yaml
```

## Create a global CIDR or IP Range

In order for `kube-vip` to set an IP address for a Service of type `LoadBalancer`, it needs to have an availability of IP address to assign. This information is stored in a Kubernetes ConfigMap to which `kube-vip` has access. You control the scope of the IP allocations with the `key` within the ConfigMap. Either CIDR blocks or IP ranges may be specified and scoped either globally (cluster-side) or per-Namespace.

To allow a global (cluster-wide) CIDR block which `kube-vip` can use to allocate an IP to Services of type `LoadBalancer` in any Namespace, create a ConfigMap named `kubevip` with the key `cidr-global` and value equal to a CIDR block available in your environment. For example, the below command creates a global CIDR with value `192.168.0.220/29` from which `kube-vip` will allocate IP addresses.

```
kubectl create configmap -n kube-system kubevip --from-literal cidr-global=192.168.0.220/29
```

To use a global range instead, create the key `range-global` with the value set to a valid range of IP addresses. For example, the below command creates a global range using the pool `192.168.1.220-192.168.1.230`.

```
kubectl create configmap -n kube-system kubevip --from-literal range-global=192.168.1.220-192.168.1.230
```

Creating services of type `LoadBalancer` in any Namespace will now take addresses from one of the global pools defined in the ConfigMap unless a Namespace-specific pool is created.

### The Kube-Vip Cloud Provider ConfigMap

To manage the IP address ranges for Services of type `LoadBalancer`, the `kube-vip-cloud-provider` uses a ConfigMap held in the `kube-system` Namespace. IP addresses can be configured using one or multiple formats:

- CIDR blocks
- IP ranges [start address - end address]
- Multiple pools by CIDR per Namespace
- Multiple IP ranges per Namespace (handles overlapping ranges)
- Setting of static addresses through service.metadata.annotations `kube-vip.io/loadbalancerIPs`
- Setting of static addresses through --load-balancer-ip=x.x.x.x (`kubectl expose` command)

To control which IP address range is used for which Service, the following rules are applied:

- Global address pools (`cidr-global` or `range-global`) are available for use by *any* Service in *any* Namespace
- Namespace specific address pools (`cidr-<namespace>` or `range-<namespace>`) are *only* available for use by a Service in the *specific* Namespace
- Static IP addresses can be applied to a Service of type `LoadBalancer` using the `spec.loadBalancerIP` field, even outside of the assigned ranges

Example Configmap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kubevip
  namespace: kube-system
data:
  cidr-default: 192.168.0.200/29                      # CIDR-based IP range for use in the default Namespace
  range-development: 192.168.0.210-192.168.0.219      # Range-based IP range for use in the development Namespace
  cidr-finance: 192.168.0.220/29,192.168.0.230/29     # Multiple CIDR-based ranges for use in the finance Namespace
  cidr-global: 192.168.0.240/29                       # CIDR-based range which can be used in any Namespace
```

### Expose a Service

We can now expose a Service and once the cloud provider has provided an address, `kube-vip` will start to advertise that address to the outside world as shown below:

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx
```

or via a Service YAML definition:

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

We can also expose a specific address by specifying it imperatively in the Service definition:


```
apiVersion: v1
kind: Service
metadata:
  annotations:
    "kube-vip.io/loadbalancerIPs": "1.1.1.1"
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

Or set it through command line.

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx --load-balancer-ip=1.1.1.1
```

Since k8s 1.24, loadbalancerIP field [is deprecated](https://github.com/kubernetes/kubernetes/pull/107235). It's recommended to use the annotations instead of command line or `service.spec.loadBalancerIP` to specify the ip.

### Using DHCP for Load Balancers (experimental)

With `kube-vip` > 0.2.1, it is possible to use the local network DHCP server to provide `kube-vip` with a load balancer address that can be used to access a Kubernetes service on the network.

In order to do this, we need to signify to `kube-vip` and the cloud provider that we don't need one of their managed addresses. We do this by explicitly exposing a Service on the address `0.0.0.0`. When `kube-vip` sees a Service on this address, it will create a `macvlan` interface on the host and request a DHCP address. Once this address is provided, it will assign it as the `LoadBalancer` IP and update the Kubernetes Service.

```
$ kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx-dhcp --load-balancer-ip=0.0.0.0; kubectl get svc
service/nginx-dhcp exposed
NAME         TYPE           CLUSTER-IP      EXTERNAL-IP     PORT(S)        AGE
kubernetes   ClusterIP      10.96.0.1       <none>          443/TCP        17m
nginx-dhcp   LoadBalancer   10.97.150.208   0.0.0.0         80:31184/TCP   0s

{ ... a second or so later ... }

$ kubectl get svc
NAME         TYPE           CLUSTER-IP      EXTERNAL-IP     PORT(S)        AGE
kubernetes   ClusterIP      10.96.0.1       <none>          443/TCP        17m
nginx-dhcp   LoadBalancer   10.97.150.208   192.168.0.155   80:31184/TCP   3s
```

### Using UPnP to expose a Service to the outside world

With `kube-vip` > 0.2.1, it is possible to expose a Service of type `LoadBalancer` on a specific port to the Internet by using UPnP (on a supported gateway).

Most simple networks look something like the following:

`<----- <internal network 192.168.0.0/24> <Gateway / router> <external network address> ----> Internet`

Using UPnP we can create a matching port on the `<external network address>` allowing your Service to be exposed to the Internet.

#### Enable UPnP

Add the following to the `kube-vip` `env:` section of either the static Pod or DaemonSet for `kube-vip`, and the rest should be completely automated.

**Note** some environments may require (Unifi) `Secure mode` being `disabled` (this allows a host with a different address to register a port).

```
- name: enableUPNP
  value: "true"
```

#### Exposing a Service

To expose a port successfully, we'll need to change the command slightly:

`--target-port=80` the port of the application in the pods (HTT/NGINX)
`--port=32380` the port the Service will be exposed on (and what you should connect to in order to receive traffic from the Service)

`kubectl expose deployment plunder-nginx --port=32380 --target-port=80 --type=LoadBalancer --namespace plunder`

The above example should expose a port on your external (Internet facing) address that can be tested externally with:

```
$ curl externalIP:32380
<!DOCTYPE html>
<html>
...
```
