# Kube-Vip Flag / Environment Variable reference

## Flags

These flags are typically used in the `kube-vip` manifest generation process.

| Category            | Flag                   | Usage                                                              | Notes                                                                           |
| ------------------- | ---------------------- | ------------------------------------------------------------------ | ------------------------------------------------------------------------------- |
| **Troubleshooting** |                        |                                                                    |                                                                                 |
|                     | `--log`                | default 4                                                          | Set to `5` for debugging logs                                                   |
| **Mode**            |                        |                                                                    |                                                                                 |
|                     | `--controlplane`       | Enables `kube-vip` control plane functionality                     |                                                                                 |
|                     | `--services`           | Enables `kube-vip` to watch services of type `LoadBalancer`        |                                                                                 |
| **VIP Config**      |                        |                                                                    |                                                                                 |
|                     | `--arp`                | Enables ARP broadcasts from Leader                                 |                                                                                 |
|                     | `--bgp`                | Enables BGP peering from `kube-vip`                                |                                                                                 |
|                     | `--vip`                | `<IP Address>`                                                     | (deprecated)                                                                    |
|                     | `--address`            | `<IP Address>` or `<DNS name>`                                     |                                                                                 |
|                     | `--interface`          | Linux interface on the node                                        |                                                                                 |
|                     | `--leaderElection`     | Enables Kubernetes LeaderElection                                  | Used by ARP, as only the leader can broadcast                                   |
|                     | `--enableLoadBalancer` | Enables IPVS load balancer                                         | `kube-vip` ≥ 0.4.0                                                              |
|                     | `--lbPort`             | 6443                                                               | The port that the api server will load-balanced on                              |
|                     | `--lbForwardingMethod` | Select the forwarding method (default local)                       | The IPVS forwarding method (local, masquerade, tunnel, directroute, bypass)     |
| **Services**        |                        |                                                                    |                                                                                 |
|                     | `--serviceInterface`   | ""                                                                 | Defines an optional different interface to bind services too                    |
|                     | `--cidr`               | Defaults "32"                                                      | Used when advertising BGP addresses (typically as `x.x.x.x/32`)                 |
| **Kubernetes**      |                        |                                                                    |                                                                                 |
|                     | `--inCluster`          | Required for `kube-vip` as DaemonSet.                              |  Runs `kube-vip` with a ServiceAccount called `kube-vip`.                       |
|                     | `--taint`              | Required for `kube-vip` as DaemonSet.                              |  Adds node affinity rules forcing `kube-vip` Pods to run on control plane.      |
| **LeaderElection**  |                        |                                                                    |                                                                                 |
|                     | `--leaseDuration`      | default 5                                                          | Seconds a lease is held for                                                     |
|                     | `--leaseRenewDuration` | default 3                                                          | Seconds a leader can attempt to renew the lease                                 |
|                     | `--leaseRetry`         | default 1                                                          | Number of times the leader will hold the lease for                              |
|                     | `--namespace`          | "kube-vip"                                                         | The namespace where the lease will reside                                       |
| **BGP**             |                        |                                                                    |                                                                                 |
|                     | `--bgpRouterID`        | `<IP Address>`                                                     | Typically the address of the local node                                         |
|                     | `--localAS`            | default 65000                                                      | The AS we peer from                                                             |
|                     | `--bgppeers`           | `<address:AS:password:multihop>`                                   | Comma separated list of BGP peers                                               |
|                     | `--peerAddress`        | `<IP Address>`                                                     | Address of a single BGP Peer                                                    |
|                     | `--peerAS`             | default 65000                                                      | AS of a single BGP Peer                                                         |
|                     | `--peerPass`           | ""                                                                 | Password to work with a single BGP Peer                                         |
|                     | `--multiHop`           | Enables eBGP MultiHop                                              | Enable multiHop with a single BGP Peer                                          |
|                     | `--sourceif`           | Source Interface                                                   | Determines which interface BGP should peer _from_                               |
|                     | `--sourceip`           | Source Address                                                     | Determines which IP address BGP should peer _from_                              |
|                     | `--annotations`        | `<provider string>`                                                | Startup will be paused until the node annotations contain the BGP configuration |
| **Equinix Metal**   |                        |                                                                    | (May be deprecated)                                                             |
|                     | `--metal`              | Enables Equinix Metal API calls                                    |                                                                                 |
|                     | `--metalKey`           | Equinix Metal API token                                            |                                                                                 |
|                     | `--metalProject`       | Equinix Metal Project (Name)                                       |                                                                                 |
|                     | `--metalProjectID`     | Equinix Metal Project (UUID)                                       |                                                                                 |
|                     | `--provider-config`    | Path to the Equinix Metal provider configuration                   | Requires the Equinix Metal CCM                                                  |

## Environment Variables

These environment variables are usually part of a `kube-vip` manifest and used when running the `kube-vip` Pod.

More environment variables can be read through the `pkg/kubevip/config_envvar.go` file.

| Category            | Environment Variable   | Usage                                                       | Notes                                                                           |
| ------------------- | ---------------------- |-------------------------------------------------------------|---------------------------------------------------------------------------------|
| **Troubleshooting** |                        |                                                             |                                                                                 |
|                     | `vip_loglevel`         | default 4                                                   | Set to `5` for debugging logs                                                   |
| **Mode**            |                        |                                                             |                                                                                 |
|                     | `cp_enable`            | Enables `kube-vip` control plane functionality              |                                                                                 |
|                     | `svc_enable`           | Enables `kube-vip` to watch Services of type `LoadBalancer` |                                                                                 |
| **VIP Config**      |                        |                                                             |                                                                                 |
|                     | `vip_arp`              | Enables ARP broadcasts from Leader                          |                                                                                 |
|                     | `bgp_enable`           | Enables BGP peering from `kube-vip`                         |                                                                                 |
|                     | `vip_address`          | `<IP Address>`                                              | (deprecated)                                                                    |
|                     | `address`              | `<IP Address>` or `<DNS name>`                              |                                                                                 |
|                     | `vip_interface`        | `<linux interface>`                                         |                                                                                 |
|                     | `vip_leaderelection`   | Enables Kubernetes LeaderElection                           | Used by ARP, as only the leader can broadcast                                   |
|                     | `lb_enable`            | Enables IPVS LoadBalancer                                   | `kube-vip` ≥ 0.4.0. Adds nodes to the IPVS load balancer                        |
|                     | `lb_port`              | 6443                                                        | The IPVS port that will be used to load-balance control plane requests          |
|                     | `lb_fwdmethod`         | Select the forwarding method (default local)                | The IPVS forwarding method (local, masquerade, tunnel, directroute, bypass)     |
| **Services**        |                        |                                                             |                                                                                 |
|                     | `vip_servicesinterface`| ""                                                          | Defines an optional different interface to bind services too                    |
|                     | `vip_cidr`             | Defaults "32"                                               | Used when advertising BGP addresses (typically as `x.x.x.x/32`)                 |
| **LeaderElection**  |                        |                                                             |                                                                                 |
|                     | `vip_leaseduration`    | default 5                                                   | Seconds a lease is held for                                                     |
|                     | `vip_renewdeadline`    | default 3                                                   | Seconds a leader can attempt to renew the lease                                 |
|                     | `vip_retryperiod`      | default 1                                                   | Number of times the leader will hold the lease for                              |
|                     | `cp_namespace`         | "kube-vip"                                                  | The namespace where the lease will reside                                       |
| **BGP**             |                        |                                                             |                                                                                 |
|                     | `bgp_routerid`         | `<IP Address>`                                              | Typically the address of the local node                                         |
|                     | `bgp_routerinterface`  | Interface name                                              | Used to associate the `routerID` with the control plane's interface.            |
|                     | `bgp_as`               | default 65000                                               | The AS we peer from                                                             |
|                     | `bgp_peers`            | `<address:AS[:password[:multihop]]>`                        | Comma separated list of BGP peers (IPv6 addresses should be enclosed with `[]`) |
|                     | `bgp_peeraddress`      | `<IP Address>`                                              | Address of a single BGP Peer                                                    |
|                     | `bgp_peeras`           | default 65000                                               | AS of a single BGP Peer                                                         |
|                     | `bgp_peerpass`         | ""                                                          | Password to work with a single BGP Peer                                         |
|                     | `bgp_multihop`         | Enables eBGP MultiHop                                       | Enable multiHop with a single BGP Peer                                          |
|                     | `bgp_sourceif`         | Source Interface                                            | Determines which interface BGP should peer _from_                               |
|                     | `bgp_sourceip`         | Source Address                                              | Determines which IP address BGP should peer _from_                              |
|                     | `bgp_hold_time`        | default 0                                                   | BGP timer HoldTime config. Defaults to 0, using the peer's default              |
|                     | `bgp_keepalive_interval`| default 0                                                  | BGP timer KeepaliveInterval config. Defaults to 0, using peer's default         |
|                     | `annotations`          | `<provider string>`                                         | Startup will be paused until the node annotations contain the BGP configuration |
| **Equinix Metal**   |                        |                                                             | (May be deprecated)                                                             |
|                     | `vip_packet`           | Enables Equinix Metal API calls                             |                                                                                 |
|                     | `PACKET_AUTH_TOKEN`    | Equinix Metal API token                                     |                                                                                 |
|                     | `vip_packetproject`    | Equinix Metal Project (Name)                                |                                                                                 |
|                     | `vip_packetprojectid`  | Equinix Metal Project (UUID)                                |                                                                                 |
|                     | `provider_config`      | Path to the Equinix Metal provider configuration            | Requires the Equinix Metal CCM                                                  |
