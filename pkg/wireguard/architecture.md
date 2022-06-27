# Wireguard Architecture

This brief document is largely for my own notes about how this functionality is added to `kube-vip`.

## Overview

- New Flags
- Startup
- Secret(s)

### New Flags

A `--wireguard` flag or `vip_wireguard` environment variable will determine if the Wireguard mode is enabled, if this is the case then it will start the wireguard manager process.

### Startup

This will require `kube-vip` starting as a daemonset as it will need to read existing data (secrets) from inside the cluster.

### Secrets 

Create a private key for the cluster:

```
PRIKEY=$(wg genkey)
PUBKEY=$(echo $PRIKEY | wg pubkey)
PEERKEY=$(sudo wg show wg0 public-key)
echo "kubectl create -n kube-system secret generic wireguard --from-literal=privateKey=$PRIKEY --from-literal=peerPublicKey=$PEERKEY --from-literal=peerEndpoint=192.168.0.179"
sudo wg set wg0 peer $PUBKEY allowed-ips 10.0.0.0/8
```
