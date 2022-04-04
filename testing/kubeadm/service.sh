#!/bin/bash
set -e

# Read node configuration
source ./testing/nodes

# Read logging function
source ./testing/logging.bash


logr "INFO"  "Starting kube-vip.io service testing with Kubeadm"
logr "DEFAULT"  "Creating Logfile $logfile"

# Adding Controller
logr "INFO" "Creating network range configmap"
ssh $NODE01 "kubectl create configmap -n kube-system kubevip --from-literal range-global=192.168.0.220-192.168.0.222" >> $logfile

logr "INFO" "Deploying kube-vip.io Controller"
ssh $NODE01 "kubectl apply -f https://raw.githubusercontent.com/kube-vip/kube-vip-cloud-provider/main/manifest/kube-vip-cloud-controller.yaml" >> $logfile

logr "INFO" "Creating \"nginx\" deployment"
ssh $NODE01 "kubectl apply -f https://k8s.io/examples/application/deployment.yaml" >> $logfile
sleep 5

logr "DEFAULT" "Creating \"nginx\" service"
ssh $NODE01 "kubectl expose deployment nginx-deployment --port=80 --type=LoadBalancer --name=nginx" >> $logfile

logr "INFO" "Sleeping for 20 seconds to give the controller time to \"reconcile\""
sleep 20

logr "INFO" "Retrieving logs from kube-vip.io cloud provider"
ssh $NODE01 "kubectl logs -n kube-system   kube-vip-cloud-provider-0" >> $logfile
logr "INFO" "Retrieving service configuration"
ssh $NODE01 "kubectl describe svc nginx" | tee >> $logfile
