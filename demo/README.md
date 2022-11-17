# Demo client-server

This contains some example code to determine how long "failovers" are taking within kube-vip, the server component should live within the cluster and the client should be externally.

## Deploy the server

Simply apply the manifest to a working cluster that has kube-vip deployed:

```
kubectl apply -f ./demo/server/deploy.yaml
```

Retrieve the loadBalancer IP that is fronting the service:

```
kubectl get svc demo-service
NAME           TYPE           CLUSTER-IP      EXTERNAL-IP     PORT(S)           AGE
demo-service   LoadBalancer   10.104.18.147   192.168.0.217   10002:32529/UDP   117m
```

## Connect the client

From elsewhere, clone the kube-vip repository and connect the client to the server endpoint (loadBalancer IP) with the following command:

```
go run ./demo/client/main.go -address=<vip>
```

You will only see output when the client has reconcilled the connection to a pod beneath the service, where it will print the timestamp to reconnection along with the time in milliseconds it took:

```
15:58:35.916952 3008
15:58:45.947506 2005
15:58:57.983151 3007
15:59:08.013450 2005
15:59:20.046491 3008
15:59:30.076341 2507
15:59:42.110747 3008
```

## Kill some pods to test

On a machine or control plane that has `kubectl` and has the credentials to speak to the cluster we will run a command to find the demo pod and kill it every 10 seconds:

`while true ; do kubectl delete pod $(kubectl get pods | grep -v NAME | grep vip| awk '{ print $1 }'); sleep 10; done`

