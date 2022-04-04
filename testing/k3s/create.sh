#!/bin/bash
 
set -e

# Read node configuration
source ./testing/nodes

# Read logging function
source ./testing/logging.bash

## Main() 

# Ensure we have an entirely new logfile
reset_logfile

logr "INFO"  "Starting kube-vip.io testing with k3s"
logr "DEFAULT"  "Creating Logfile $logfile"

if [[ -z $1 && -z $2  && -z $3 && -z $4 ]]; then
    echo "Usage:"
    echo "   Param 1: Kube-Vip Version"
    echo "   Param 2: Kube-Vip mode [\"controlplane\"/\"services\"/\"hybrid\"]"
    echo "   Param 3: Vip address"
    echo "   Param 4: k3s url (https://github.com/k3s-io/k3s/releases/download/v1.20.4%2Bk3s1/k3s)"
    echo "" ./create.sh 0.3.3 hybrid 192.168.0.40 
    exit 1
fi

# Sane variable renaming
kubernetes_version=$4
kube_vip_version=$1
kube_vip_vip=$3

case "$2" in
"controlplane")  logr "INFO" "Creating in control plane only mode"
    kube_vip_mode="--controlplane"
    ;;
"services")  logr "INFO"  "Creating in services-only mode"
    kube_vip_mode="--services"
    ;;
"hybrid")  logr "INFO"  "Creating in hybrid mode"
    kube_vip_mode="--controlplane --services"
    ;;
*) echo "Unknown kube-vip mode [$3]"
   exit -1
   ;;
esac

ssh $NODE01 "sudo mkdir -p /var/lib/rancher/k3s/server/manifests/"
ssh $NODE01 "sudo docker run --network host --rm plndr/kube-vip:$kube_vip_version manifest daemonset $kube_vip_mode --interface ens160 --vip $kube_vip_vip --arp --leaderElection --inCluster --taint | sudo tee /var/lib/rancher/k3s/server/manifests/vip.yaml"
ssh $NODE01 "sudo curl https://kube-vip.io/manifests/rbac.yaml | sudo tee /var/lib/rancher/k3s/server/manifests/rbac.yaml"
ssh $NODE01 "sudo screen -dmSL k3s k3s server --cluster-init --tls-san $kube_vip_vip --no-deploy servicelb --disable-cloud-controller --token=test"
echo "Started first node, sleeping for 60 seconds"
sleep 60
echo "Adding additional nodes"
ssh $NODE02 "sudo screen -dmSL k3s k3s server --server https://$kube_vip_vip:6443 --token=test"
ssh $NODE03 "sudo screen -dmSL k3s k3s server --server https://$kube_vip_vip:6443 --token=test"
sleep 20
ssh $NODE01 "sudo k3s kubectl get node -o wide"
