# Kube-vip on KIND

## Deploying KIND

The documentation for KIND is fantastic and it's quickstart guide will have you up and running in no time -> [https://kind.sigs.k8s.io/docs/user/quick-start/](https://kind.sigs.k8s.io/docs/user/quick-start/)

## Find Address Pool for Kube-Vip

We will need to find addresses that can be used by Kube-Vip:

```
docker network inspect kind -f '{{ range $i, $a := .IPAM.Config }}{{ println .Subnet }}{{ end }}'
```

This will return a cidr range such as `172.18.0.0/16` and from here we can select a range.

## Deploy the Kube-Vip Cloud Controller

```
$ kubectl apply -f https://raw.githubusercontent.com/kube-vip/kube-vip-cloud-provider/main/manifest/kube-vip-cloud-controller.yaml
```

## Add our Address range

```
kubectl create configmap --namespace kube-system kubevip --from-literal range-global=172.18.100.10-172.18.100.30
```

## Install kube-vip

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

## Deploy Kube-vip as a deamonset

```
kube-vip manifest daemonset --services --inCluster --arp --interface eth0 | ./kubectl apply -f -
```

## Test

```
kubectl apply -f https://k8s.io/examples/application/deployment.yaml
```

```
kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx
```

```
kubectl get svc
NAME         TYPE           CLUSTER-IP      EXTERNAL-IP     PORT(S)        AGE
kubernetes   ClusterIP      10.96.0.1       <none>          443/TCP        74m
nginx        LoadBalancer   10.96.196.235   172.18.100.11   80:31236/TCP   6s
```