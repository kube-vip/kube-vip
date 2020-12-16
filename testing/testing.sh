#!/bin/bash

make build
./kube-vip manifest pod --interface eth0 --vip 192.168.0.1 --controlplane --arp --services --leaderElection
./kube-vip manifest pod --interface eth0 --vip 192.168.0.1 --controlplane --bgp
./kube-vip manifest pod --interface eth0 --vip 192.168.0.1 --controlplane --bgp --bgppeers 192.168.0.2:12345::true,192.168.0.3:12345::true
./kube-vip manifest pod --interface eth0 --vip 192.168.0.1 --controlplane --arp --leaderElection
rm ./kube-vip