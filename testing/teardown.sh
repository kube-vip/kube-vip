#!/bin/bash
echo "Wiping Nodes in reverse order, and rebooting"
ssh k8s05 "sudo kubeadm reset -f; sudo rm -rf /etc/cni/net.d; sudo reboot"
ssh k8s04 "sudo kubeadm reset -f; sudo rm -rf /etc/cni/net.d; sudo reboot"
ssh k8s03 "sudo kubeadm reset -f; sudo rm -rf /etc/cni/net.d; sudo reboot"
ssh k8s02 "sudo kubeadm reset -f; sudo rm -rf /etc/cni/net.d; sudo reboot"
ssh k8s01 "sudo kubeadm reset -f --skip-phases preflight update-cluster-status remove-etcd-member; sudo rm -rf /etc/cni/net.d; sudo reboot"
echo
echo "All Control Plane Nodes have been reset"
echo "Consider removing kube-vip images if changing version"