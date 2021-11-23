#!/bin/bash

if [[ -z $1 && -z $2  && -z $3 && -z $4 ]]; then
    echo "Usage:"
    echo "   Param 1: Kubernetes Version"
    echo "   Param 2: Kube-Vip Version"
    echo "   Param 3: Kube-Vip mode [\"controlplane\"/\"services\"/\"hybrid\"]"
    echo "   Param 4: Vip address"
    echo ""
    echo "" ./create_k8s.sh 1.18.5 0.4.0 hybrid 192.168.0.40
    exit 1
fi

case "$3" in

"controlplane")  echo "Creating in control plane only mode"
    mode="--controlplane"
    ;;
"services")  echo  "Creating in services-only mode"
    mode="--services"
    ;;
"hybrid")  echo  "Creating in hybrid mode"
    mode="--controlplane --services"
    ;;
*) echo "Unknown kube-vip mode [$3]"
   exit -1
   ;;
esac

install_deps() {
    echo "Installing Kubernetes dependencies for Kubernetes $! on all nodes"
    ssh $NODE01 "curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy kubelet=$1-00 kubectl=$1-00 kubeadm=$1-00"
    ssh $NODE02 "curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy kubelet=$1-00 kubectl=$1-00 kubeadm=$1-00"
    ssh $NODE03 "curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy kubelet=$1-00 kubectl=$1-00 kubeadm=$1-00"
    ssh $NODE04 "curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy kubelet=$1-00 kubectl=$1-00 kubeadm=$1-00"
    ssh $NODE05 "curl -s https://packages.cloud.google.com/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy kubelet=$1-00 kubectl=$1-00 kubeadm=$1-00"
}

source ./testing/nodes

first_node() {
echo "Creating First node!"
#ssh $NODE01  "sudo modprobe ip_vs_rr"
#ssh $NODE01  "sudo modprobe nf_conntrack"
ssh $NODE01  "docker rmi plndr/kube-vip:dev"

# echo "echo "ip_vs | tee -a /etc/modules"
ssh $NODE01 "sudo docker run --network host --rm plndr/kube-vip:$2 manifest pod  --interface ens160 --vip $4 --arp --leaderElection --enableLoadBalancer $5 | sed  \"s/image:.*/image: plndr\/kube-vip:dev/\" | sudo tee /etc/kubernetes/manifests/vip.yaml"
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
}


additional_controlplane() {
ssh $NODE02 "sudo $JOIN_CMD $CONTROLPLANE_CMD"
sleep 3
ssh $NODE02 "sudo docker run --network host --rm plndr/kube-vip:$1 manifest pod --interface ens160 --vip $2 --arp --leaderElection --enableLoadBalancer $3 | sed  \"s/image:.*/image: plndr\/kube-vip:dev/\"| sudo tee /etc/kubernetes/manifests/vip.yaml"

ssh $NODE03 "sudo $JOIN_CMD $CONTROLPLANE_CMD"
sleep 3
ssh $NODE03 "sudo docker run --network host --rm plndr/kube-vip:$1 manifest pod --interface ens160 --vip $2 --arp --leaderElection --enableLoadBalancer $3 | sed  \"s/image:.*/image: plndr\/kube-vip:dev/\"| sudo tee /etc/kubernetes/manifests/vip.yaml"
}

first_node $1 $2 $3 $4 $mode
additional_controlplane $2 $4 $mode

ssh $NODE04 "sudo $JOIN_CMD"
ssh $NODE05 "sudo $JOIN_CMD"
echo
echo " Nodes should be deployed at this point, waiting 5 secs and querying the deployment"
sleep 5
ssh $NODE01 "kubectl get nodes"
ssh $NODE01 "kubectl get pods -A"
echo 
echo "Kubernetes: $1, Kube-vip $2, Advertising VIP: $4"
