#!/bin/bash

if [[ -z $1  ]]; then
    echo "Usage:"
    echo "   Param 1: kube-vip Version"
    exit 1
fi

echo "Removing docker images from workers"
sleep 2
ssh k8s05 "sudo docker rmi plndr/kube-vip:$1"
ssh k8s04 "sudo docker rmi plndr/kube-vip:$1" 
ssh k8s03 "sudo docker rmi plndr/kube-vip:$1" 
ssh k8s02 "sudo docker rmi plndr/kube-vip:$1" 
ssh k8s01 "sudo docker rmi plndr/kube-vip:$1" 
