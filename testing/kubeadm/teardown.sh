#!/bin/bash

source ./testing/nodes

echo "Wiping Nodes in reverse order, and rebooting"
ssh $NODE05 "sudo kubeadm reset -f; sudo rm -rf /etc/cni/net.d; sudo reboot"
ssh $NODE04 "sudo kubeadm reset -f; sudo rm -rf /etc/cni/net.d; sudo reboot"
ssh $NODE03 "sudo kubeadm reset -f; sudo rm -rf /etc/cni/net.d; sudo reboot"
ssh $NODE02 "sudo kubeadm reset -f; sudo rm -rf /etc/cni/net.d; sudo reboot"
ssh $NODE01 "sudo kubeadm reset -f --skip-phases preflight update-cluster-status remove-etcd-member; sudo rm -rf /etc/cni/net.d; sudo reboot"
echo
echo "All Control Plane Nodes have been reset"
echo "Consider removing kube-vip images if changing version"