#!/bin/bash

abort()
{
    echo >&2 '
***************
*** ABORTED ***
***************
'
    echo "An error occurred. Exiting..." >&2
    exit 1
}

trap 'abort' 0

set -e
set -o pipefail

echo "==> ARP w/services & controlplane"
docker run --network host --rm plndr/kube-vip:action manifest pod --interface eth0 --vip 192.168.0.1 --controlplane --arp --services --leaderElection

echo "==> BGP w/controlplane"
docker run --network host --rm plndr/kube-vip:action manifest pod --interface eth0 --vip 192.168.0.1 --controlplane --bgp

echo "==> BGP w/controlplane and specified peers"
docker run --network host --rm plndr/kube-vip:action manifest pod --interface eth0 --vip 192.168.0.1 --controlplane --bgp --bgppeers 192.168.0.2:12345::true,192.168.0.3:12345::true

echo "==> BGP w/services & controlplane"
docker run --network host --rm plndr/kube-vip:action manifest pod --interface eth0 --vip 192.168.0.1 --controlplane --arp --leaderElection

echo "==> ARP w/controlplane (using --address)"
docker run --network host --rm plndr/kube-vip:action manifest pod --interface enx001e063262b1 --address k8s-api-vip.lan --arp --leaderElection --controlplane

echo "==> ARP w/controlplane (using --address)"
docker run --network host --rm plndr/kube-vip:action manifest daemonset  --interface eth0 --vip 192.168.0.1   --controlplane \
    --services \
    --inCluster \
    --taint 

trap : 0

echo >&2 '
************
*** DONE *** 
************
'
