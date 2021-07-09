# Kube-vip (Layer 3 / BGP)

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

Ensure the `vip_arp` isn't enabled as ARP and BGP can't be used at the same time (today), also that the `vip_interface` is set to localhost (`lo`).

```
        - name: vip_interface
		  value: "lo"
		- name: vip_configmap
		  value: "plndr" 
		- name: bgp_enable
		  value: "true"
		- name: vip_loglevel
		  value: "5"
```

### BGP Specific configuration

Additionally for BGP we'll need some configuration details, your local friendly network admin should be able to help here:

```
		- name: bgp_routerid
		  value: "192.168.0.45"
		- name: bgp_as
          value: "65000" 
		- name: bgp_peeraddress
    	  value: "10.0.0.1"
		- name: bgp_peeras
    	  value: "65522"
```

### BGP on Packet

If you're lucky enough to be running services on Packet then The above BGP information can be found from the API, instead of specifying the above we need to use the following:

```
		- name: vip_packet
		  value: "true"
		- name: vip_packetproject
		  value: "My Project" 
		- name: PACKET_AUTH_TOKEN
		  value: "XXYZZYVVY"
```

With the above configuration in place, all `kube-vip` pods will start in active mode and when a service is exposed then all nodes will advertise the VIP to the routers.

## Expose a service

Given that `kube-vip` doesn't know your network (at this point) ask your local friendly network OPs for an address you can advertise. That is the address you can expose to the outside world as shown below!

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx --load-balancer-ip=1.1.1.1
```

## Expose with packet 

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