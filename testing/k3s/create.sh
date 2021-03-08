#!/bin/bash

if [[ -z $1 && -z $2  && -z $3 && -z $4 ]]; then
    echo "Usage:"
    echo "   Param 1: Kube-Vip Version"
    echo "   Param 2: Kube-Vip mode [\"controlplane\"/\"services\"/\"hybrid\"]"
    echo "   Param 3: Vip address"
    echo "   Param 4: k3s url (https://github.com/k3s-io/k3s/releases/download/v1.20.4%2Bk3s1/k3s)"
    echo "" ./create.sh 0.3.3 hybrid 192.168.0.40 
    exit 1
fi

case "$2" in

"controlplane")  echo "Creating control plane only cluster"
    mode="--controlplane"
    ;;
"services")  echo "Creating services only cluster"
    mode="--services"
    ;;
"hybrid")  echo  "Creating hybrid cluster"
    mode="--controlplane --services"
    ;;
*) echo "Unknown kube-vip mode [$2]"
   exit -1
   ;;
esac

source ./testing/nodes

echo "Creating First node!"

ssh $NODE01 "sudo mkdir -p /var/lib/rancher/k3s/server/manifests/"
ssh $NODE01 "sudo docker run --network host --rm plndr/kube-vip:$1 manifest daemonset $mode --interface ens160 --vip $3 --arp --leaderElection --inCluster --taint | sudo tee /var/lib/rancher/k3s/server/manifests/vip.yaml"
ssh $NODE01 "sudo curl https://kube-vip.io/manifests/rbac.yaml | sudo tee /var/lib/rancher/k3s/server/manifests/rbac.yaml"
ssh $NODE01 "sudo screen -dmSL k3s k3s server --cluster-init --tls-san $3 --no-deploy servicelb --disable-cloud-controller --token=test"
echo "Started first node, sleeping for 60 seconds"
sleep 60
echo "Adding additional nodes"
ssh $NODE02 "sudo screen -dmSL k3s k3s server --server https://$3:6443 --token=test"
ssh $NODE03 "sudo screen -dmSL k3s k3s server --server https://$3:6443 --token=test"
sleep 20
ssh $NODE01 "sudo k3s kubectl get node -o wide"
