#!/bin/bash

if [[ -z $1 && -z $2  && -z $3 && -z $4 ]]; then
    echo "Usage:"
    echo "   Param 1: Kubernetes Version"
    echo "   Param 2: Kube-Vip Version"
    echo "   Param 3: Kube-Vip mode [\"controlplane\"/\"services\"/\"hybrid\"]"
    echo "   Param 4: Vip address"
    echo ""
    echo "" ./create_k8s.sh 1.18.5 0.3.3 192.168.0.40
    exit 1
fi

case "$3" in

"controlplane")  echo "Sending SIGHUP signal"
    mode="--controlplane"
    ;;
"services")  echo  "Sending SIGINT signal"
    mode="--services"
    ;;
"hybrid")  echo  "Sending SIGQUIT signal"
    mode="--controlplane --services"
    ;;
*) echo "Unknown kube-vip mode [$3]"
   exit -1
   ;;
esac

source ./testing/nodes

echo "Creating First node!"

ssh $NODE01 "sudo docker run --network host --rm plndr/kube-vip:$2 manifest pod $mode --interface ens160 --vip $4 --arp --leaderElection | sudo tee /etc/kubernetes/manifests/vip.yaml"
CONTROLPLANE_CMD=$(ssh $NODE01 "sudo kubeadm init --kubernetes-version $1 --control-plane-endpoint $4 --upload-certs --pod-network-cidr=10.0.0.0/16 | grep certificate-key")
ssh $NODE01 "sudo rm -rf ~/.kube/"
ssh $NODE01 "mkdir -p .kube"
ssh $NODE01 "sudo cp -i /etc/kubernetes/admin.conf .kube/config"
ssh $NODE01 "sudo chown dan:dan .kube/config"
echo "Enabling strict ARP on kube-proxy"
ssh $NODE01 "kubectl get configmap kube-proxy -n kube-system -o yaml | sed -e \"s/strictARP: false/strictARP: true/\" | kubectl apply -f - -n kube-system"
ssh $NODE01 "kubectl describe configmap -n kube-system kube-proxy | grep strictARP"
ssh $NODE01 "kubectl apply -f https://docs.projectcalico.org/manifests/calico.yaml"
JOIN_CMD=$(ssh $NODE01 " sudo kubeadm token create --print-join-command 2> /dev/null")

ssh $NODE02 "sudo $JOIN_CMD $CONTROLPLANE_CMD"
sleep 3
ssh $NODE02 "sudo docker run --network host --rm plndr/kube-vip:$2 manifest pod --interface ens160 --vip $4 --arp --leaderElection $mode | sudo tee /etc/kubernetes/manifests/vip.yaml"

ssh $NODE03 "sudo $JOIN_CMD $CONTROLPLANE_CMD"
sleep 3
ssh $NODE03 "sudo docker run --network host --rm plndr/kube-vip:$2 manifest pod --interface ens160 --vip $4 --arp --leaderElection $mode | sudo tee /etc/kubernetes/manifests/vip.yaml"
ssh $NODE04 "sudo $JOIN_CMD"
ssh $NODE05 "sudo $JOIN_CMD"
echo
echo " Nodes should be deployed at this point, waiting 5 secs and querying the deployment"
sleep 5
ssh $NODE01 "kubectl get nodes"
ssh $NODE01 "kubectl get pods -A"
echo 
echo "Kubernetes: $1, Kube-vip $2, Advertising VIP: $4"