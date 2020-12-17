#!/bin/bash

if [[ -z $1 && -z $2  && -z $3 ]]; then
    echo "Usage:"
    echo "   Param 1: Kubernetes Version"
    echo "   Param 2: Kube-Vip Version"
    echo "   Param 3: Vip address"
    echo ""
    echo "" ./create_k8s.sh 1.18.5 0.1.8 192.168.0.41
    exit 1
fi
 
echo "Creating First node!"
ssh k8s01 "sudo docker run --network host --rm plndr/kube-vip:$2 manifest pod --controlplane --interface ens160 --vip $3 --arp --leaderElection | sudo tee /etc/kubernetes/manifests/vip.yaml"
CONTROLPLANE_CMD=$(ssh k8s01 "sudo kubeadm init --kubernetes-version $1 --control-plane-endpoint $3 --upload-certs --pod-network-cidr=10.0.0.0/16 | grep certificate-key")
ssh k8s01 "sudo rm -rf ~/.kube/"
ssh k8s01 "mkdir -p .kube"
ssh k8s01 "sudo cp -i /etc/kubernetes/admin.conf .kube/config"
ssh k8s01 "sudo chown dan:dan .kube/config"
echo "Enabling strict ARP on kube-proxy"
ssh k8s01 "kubectl get configmap kube-proxy -n kube-system -o yaml | sed -e \"s/strictARP: false/strictARP: true/\" | kubectl apply -f - -n kube-system"
ssh k8s01 "kubectl describe configmap -n kube-system kube-proxy | grep strictARP"
ssh k8s01 "kubectl apply -f https://docs.projectcalico.org/manifests/calico.yaml"
JOIN_CMD=$(ssh k8s01 " sudo kubeadm token create --print-join-command 2> /dev/null")

ssh k8s02 "sudo $JOIN_CMD $CONTROLPLANE_CMD"
sleep 3
ssh k8s02 "sudo docker run --network host --rm plndr/kube-vip:$2 manifest pod --interface ens160 --vip $3 --arp --leaderElection --controlplane | sudo tee /etc/kubernetes/manifests/vip.yaml"

ssh k8s03 "sudo $JOIN_CMD $CONTROLPLANE_CMD"
sleep 3
ssh k8s03 "sudo docker run --network host --rm plndr/kube-vip:$2 manifest pod --interface ens160 --vip $3 --arp --leaderElection --controlplane | sudo tee /etc/kubernetes/manifests/vip.yaml"
ssh k8s04 "sudo $JOIN_CMD"
ssh k8s05 "sudo $JOIN_CMD"
echo
echo " Nodes should be deployed at this point, waiting 5 secs and querying the deployment"
sleep 5
ssh k8s01 "kubectl get nodes"
ssh k8s01 "kubectl get pods -A"
echo 
echo "Kubernetes: $1, Kube-vip $2, Advertising VIP: $3"