#!/bin/bash

source ./testing/nodes

echo "Wiping Nodes in reverse order, and rebooting"
ssh $NODE05 "sudo pkill k3s; sudo rm -rf /var/lib/rancher /etc/rancher; sudo reboot"
ssh $NODE04 "sudo pkill k3s; sudo rm -rf /var/lib/rancher /etc/rancher; sudo reboot"
ssh $NODE03 "sudo pkill k3s; sudo rm -rf /var/lib/rancher /etc/rancher; sudo reboot"
ssh $NODE02 "sudo pkill k3s; sudo rm -rf /var/lib/rancher /etc/rancher; sudo reboot"
ssh $NODE01 "sudo pkill k3s; sudo rm -rf /var/lib/rancher /etc/rancher; sudo reboot"
echo
echo "All Control Plane Nodes have been reset"
echo "Consider removing kube-vip images if changing version"