#!/bin/sh
echo "Executing /iptables-wrapper-installer.sh to configure iptables ..."
/iptables-wrapper-installer.sh

echo "Executing /kube-vip $@"
exec /kube-vip "$@"
