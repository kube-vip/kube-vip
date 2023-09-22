#!/bin/bash
set -e

# Read node configuration
source ./testing/nodes

# Read logging function
source ./testing/logging.bash

install_deps() {
    echo "Installing Kubernetes dependencies for Kubernetes $kubernetes_version on all nodes"
    ssh $NODE01 "sudo rm /etc/apt/sources.list.d/* && curl -4 -s -L https://dl.k8s.io/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy --allow-downgrades kubelet=$kubernetes_version-00 kubectl=$kubernetes_version-00 kubeadm=$kubernetes_version-00"
    ssh $NODE02 "sudo rm /etc/apt/sources.list.d/* && curl -4 -s -L https://dl.k8s.io/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy --allow-downgrades kubelet=$kubernetes_version-00 kubectl=$kubernetes_version-00 kubeadm=$kubernetes_version-00"
    ssh $NODE03 "sudo rm /etc/apt/sources.list.d/* && curl -4 -s -L https://dl.k8s.io/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy --allow-downgrades kubelet=$kubernetes_version-00 kubectl=$kubernetes_version-00 kubeadm=$kubernetes_version-00"
    ssh $NODE04 "sudo rm /etc/apt/sources.list.d/* && curl -4 -s -L https://dl.k8s.io/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy --allow-downgrades kubelet=$kubernetes_version-00 kubectl=$kubernetes_version-00 kubeadm=$kubernetes_version-00"
    ssh $NODE05 "sudo rm /etc/apt/sources.list.d/* && curl -4 -s -L https://dl.k8s.io/apt/doc/apt-key.gpg | sudo apt-key add && sudo apt-add-repository \"deb http://apt.kubernetes.io/ kubernetes-xenial main\" && sudo apt-get update -q && sudo apt-get install -qy --allow-downgrades kubelet=$kubernetes_version-00 kubectl=$kubernetes_version-00 kubeadm=$kubernetes_version-00"
}

first_node() {
  logr "INFO" "Creating First node!" 
  #ssh $NODE01  "sudo modprobe ip_vs_rr"
  #ssh $NODE01  "sudo modprobe nf_conntrack"
  logr "INFO" "$(ssh $NODE01  "ctr images rm ghcr.io/kube-vip/kube-vip:$kube_vip_version" 2>&1)"

  # echo "echo "ip_vs | tee -a /etc/modules"
  logr "INFO" "Creating Kube-vip.io Manifest" 

  ssh $NODE01 "sudo ctr image pull ghcr.io/kube-vip/kube-vip:$kube_vip_version"
  ssh $NODE01 "sudo mkdir -p /etc/kubernetes/manifests/; sudo ctr run --rm --net-host ghcr.io/kube-vip/kube-vip:$kube_vip_version vip /kube-vip manifest pod  --interface ens160 --vip $kube_vip_vip --arp --leaderElection --enableLoadBalancer $kube_vip_mode | sed  \"s/image:.*/image: plndr\/kube-vip:$kube_vip_version/\" | sudo tee /etc/kubernetes/manifests/vip.yaml" >> $logfile
  logr "INFO" "Deploying first Kubernetes node $NODE01"
  FIRST_NODE=$(ssh $NODE01 "sudo kubeadm init --kubernetes-version $kubernetes_version --control-plane-endpoint $kube_vip_vip --upload-certs --pod-network-cidr=10.0.0.0/16")
  echo "$FIRST_NODE" >> $logfile
  CONTROLPLANE_CMD=$(echo "$FIRST_NODE" | grep -m1 certificate-key)
  #CONTROLPLANE_CMD=$(ssh $NODE01 "sudo kubeadm init --kubernetes-version $kubernetes_version --control-plane-endpoint $kube_vip_vip --upload-certs --pod-network-cidr=10.0.0.0/16 | grep -m1 certificate-key")
  ssh $NODE01 "sudo rm -rf ~/.kube/"
  ssh $NODE01 "mkdir -p .kube"
  ssh $NODE01 "sudo cp -i /etc/kubernetes/admin.conf .kube/config"
  ssh $NODE01 "sudo chown dan:dan .kube/config"
  logr "INFO" "Enabling strict ARP on kube-proxy"
  ssh $NODE01 "kubectl get configmap kube-proxy -n kube-system -o yaml | sed -e \"s/strictARP: false/strictARP: true/\" | kubectl apply -f - -n kube-system"
  ssh $NODE01 "kubectl describe configmap -n kube-system kube-proxy | grep strictARP"
  logr "INFO" "Deploying Calico to the Kubernetes Cluster"
  ssh $NODE01 "kubectl apply -f https://docs.projectcalico.org/manifests/calico.yaml" >> $logfile
  logr "INFO" "Retrieving Join command"
  JOIN_CMD=$(ssh $NODE01 "kubeadm token create --print-join-command 2> /dev/null")
}


additional_controlplane() {
  logr "INFO" "Adding $NODE02"
  ssh $NODE02 "sudo $JOIN_CMD $CONTROLPLANE_CMD" >> $logfile
  sleep 1
  ssh $NODE02 "sudo ctr image pull ghcr.io/kube-vip/kube-vip:$kube_vip_version; sudo mkdir -p /etc/kubernetes/manifests/; sudo ctr run --rm --net-host ghcr.io/kube-vip/kube-vip:$kube_vip_version vip /kube-vip manifest pod --interface ens160 --vip $kube_vip_vip --arp --leaderElection --enableLoadBalancer $kube_vip_mode | sed  \"s/image:.*/image: plndr\/kube-vip:$kube_vip_version/\"| sudo tee /etc/kubernetes/manifests/vip.yaml" >> $logfile
  logr "INFO" "Adding $NODE03"
  ssh $NODE03 "sudo $JOIN_CMD $CONTROLPLANE_CMD" >> $logfile
  sleep 1
  ssh $NODE03 "sudo ctr image pull ghcr.io/kube-vip/kube-vip:$kube_vip_version; sudo mkdir -p /etc/kubernetes/manifests/; sudo ctr run --rm --net-host ghcr.io/kube-vip/kube-vip:$kube_vip_version vip /kube-vip manifest pod --interface ens160 --vip $kube_vip_vip --arp --leaderElection --enableLoadBalancer $kube_vip_mode | sed  \"s/image:.*/image: plndr\/kube-vip:$kube_vip_version/\"| sudo tee /etc/kubernetes/manifests/vip.yaml" >> $logfile
}

## Main() 

# Ensure we have an entirely new logfile
reset_logfile

logr "INFO"  "Starting kube-vip.io testing with Kubeadm"
logr "DEFAULT"  "Creating Logfile $logfile"

if [[ -z $1 && -z $2  && -z $3 && -z $4 ]]; then
    echo "Usage:"
    echo "   Param 1: Kubernetes Version"
    echo "   Param 2: Kube-Vip Version"
    echo "   Param 3: Kube-Vip mode [\"controlplane\"/\"services\"/\"hybrid\"]"
    echo "   Param 4: Vip address"
    echo ""
    echo "" ./create.sh 1.18.5 0.4.0 hybrid 192.168.0.40
    exit 1
fi

# Sane variable renaming
kubernetes_version=$1
kube_vip_version=$2
kube_vip_vip=$4

case "$3" in
"controlplane")  logr "INFO" "Creating in control plane only mode"
    kube_vip_mode="--controlplane"
    ;;
"services")  logr "INFO"  "Creating in services-only mode"
    kube_vip_mode="--services"
    ;;
"hybrid")  logr "INFO"  "Creating in hybrid mode"
    kube_vip_mode="--controlplane --services"
    ;;
*) echo "Unknown kube-vip mode [$3]"
   exit -1
   ;;
esac

if [[ -z "$DEPS" ]]; then
  logr "INFO" "Installing specific version of Kubernetes Dependencies"
  install_deps
fi

first_node
additional_controlplane
logr "INFO" "Adding $NODE04"
ssh $NODE04 "sudo $JOIN_CMD" >> $logfile
logr "INFO" "Adding $NODE05"
ssh $NODE05 "sudo $JOIN_CMD" >> $logfile
logr "DEFAULT" "Nodes should be deployed at this point, waiting 5 secs and querying the deployment"
echo
sleep 5
ssh $NODE01 "kubectl get nodes" | tee >> $logfile
ssh $NODE01 "kubectl get pods -A" | tee >> $logfile
echo 
logr "INFO" "Kubernetes: $kubernetes_version, Kube-vip $kube_vip_version, Advertising VIP: $kube_vip_vip"
