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

The secrets map is `[hostname]pubkey`

`kubectl create secret generic wireguard --from-literal=k8s01=abcdef12345`