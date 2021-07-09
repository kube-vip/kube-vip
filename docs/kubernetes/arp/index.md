# Kube-vip (Layer 2 / ARP)

**BEFORE** we begin we should ensure that ipvs has `strict` ARP enabled:

```
$ kubectl describe configmap -n kube-system kube-proxy | grep ARP
  strictARP: false
```

If this is false we can enable it with the command:

```
$ kubectl get configmap kube-proxy -n kube-system -o yaml | \
 sed -e "s/strictARP: false/strictARP: true/" | \
 kubectl apply -f - -n kube-system
```

and confirm with:

```
$ kubectl describe configmap -n kube-system kube-proxy | grep ARP
  strictARP: true
```

## Deploy `kube-vip`

To deploy the [latest] then `kubectl apply -f https://kube-vip.io/manifests/kube-vip.yaml`, specific versions should be found in the repository as detailed below:

From the GitHub repository [https://github.com/kube-vip/kube-vip/tree/master/example/deploy](https://github.com/kube-vip/kube-vip/tree/master/example/deploy) find the version of the `kube-vip` to deploy (although typically the highest version number will provider more functionality/stability). The [raw] option in Github will provide the url that can be applied directly with a `kubectl apply -f <url>`.

The following output should appear when the manifest is applied: 
```
serviceaccount/vip created
role.rbac.authorization.k8s.io/vip-role created
rolebinding.rbac.authorization.k8s.io/vip-role-bind created
deployment.apps/kube-vip-cluster created
```

*NOTE* The manifest for the `kube-vip` deployment has rules to ensure affinity (pods are always distributed to different nodes for HA). By default the replicas are set to `3` in the event you have less than `3` worker nodes then those replicas will sit as `pending`. This in itself isn't an issue, it means when new workers are added then they will be scheduled. *However*, tooling such as `kapps` will inspect the manifest before it's applied an error because of issues such as this.

### Editing `kube-vip` configuration

Either download and edit the manifest locally or apply as above and edit the deployment with `kubectl edit deploy/kube-vip-cluster` (change namespace where appropriate `-n`)

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
- `vip_configmap` - defines the `configmap` that `kube-vip` will watch for service configuration
- `vip_arp` - determines if ARP broadcasts are enabled
- `vip_loglevel` - determines the verbosity of logging

## Using other namespaces

In this example we'll deploy and load-balance within the namespace `plunder`

### Create the namespace

`kubectl create namespace plunder`

### Add a network range/cidr for this namespace

`kubectl edit -n kube-system configmap/plndr`

We will add the range 192.168.0.210/29 for the namespace plunder underneath the existing range for the namespace default:

```
apiVersion: v1
data:
  cidr-default: 192.168.0.200/29
  cidr-global: 192.168.0.210/29
  cidr-plunder: 192.168.0.220/29
<...>
```

### Deploy `kube-vip` in the namespace **plunder**

In the same way we deployed `kube-vip` into the default namespace we can deploy the same manifest into a different namespace using `-n namespace` e.g.

**Note** change the version of manifest when actually deploying!

```
kubectl apply -f https://kube-vip.io/manifests/kube-vip.yaml -n plunder
```

## Usage

This example will deploy into the namespace `plunder` as mention in the [Using other namespaces](Using other namespaces) example. Remove the `-n plunder` to deploy within the `default` namespace.

### Deploy nginx

```
kubectl create deployment --image nginx plunder-nginx --namespace plunder
```

### Create a load balancer

```
kubectl expose deployment plunder-nginx --port=80 --type=LoadBalancer --namespace plunder
```

## Using DHCP for Load Balancers (experimental)

With the latest release of `kube-vip` > 0.2.1, it is possible to use the local network DHCP server to provide `kube-vip` with a load-balancer address that can be used to access a Kubernetes service on the network. 

In order to do this we need to signify to `kube-vip` and the cloud-provider that we don't need one of their managed addresses. We do this by explicitly exposing a service on the address `0.0.0.0`. When `kube-vip` sees a service on this address it will create a `macvlan` interface on the host and request a DHCP address, once this address is provided it will assign it as the VIP and update the Kubernetes service!

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

## Using UPNP to expose a service to the outside world

With the latest release of `kube-vip` > 0.2.1, it is possible to expose a load-balancer on a specific port and using UPNP (on a supported gateway) expose this service to the internet.

Most simple networks look something like the following:

`<----- <internal network 192.168.0.0/24> <Gateway / router> <external network address> ----> Internet`

Using UPNP we can create a matching port on the `<external network address>` allowing your service to be exposed to the internet.

### Enable UPNP

Add the following to the `kube-vip` `env:` section, and the rest should be completely automated. 

**Note** some environments may require (Unifi) will require `Secure mode` being `disabled` (this allows a host with a different address to register a port)

```
- name: enableUPNP
  value: "true"
```

### Exposing a service

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

## Troubleshooting

Typically the logs from the `kube-vip` controller will reveal the most clues as to where a problem may lie.

The `ClusterRoleBinding` is missing will result in the following:

```
E0229 17:36:38.014351       1 retrywatcher.go:129] Watch failed: unknown (get endpoints)
E0229 17:36:38.014352       1 retrywatcher.go:129] Watch failed: unknown (get endpoints)
```

Additionally ensure that the vip_interface matches the correct interface from `ip addr`