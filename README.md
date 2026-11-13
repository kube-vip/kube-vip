# kube-vip

High Availability and Load-Balancing

![](https://github.com/kube-vip/kube-vip/raw/main/kube-vip.png)

[![Build and publish main image regularly](https://github.com/kube-vip/kube-vip/actions/workflows/main.yaml/badge.svg)](https://github.com/kube-vip/kube-vip/actions/workflows/main.yaml) [![LFX Health Score](https://img.shields.io/static/v1?label=Health%20Score&message=Healthy&color=A7F3D0&logo=linuxfoundation&logoColor=white&style=flat)](https://insights.linuxfoundation.org/project/kube-vip) [![LFX Active Contributors](https://img.shields.io/static/v1?label=Active%20contributors%20(1Y)&message=212&color=0094FF&logo=linuxfoundation&logoColor=white&style=flat)](https://insights.linuxfoundation.org/project/kube-vip)

## Overview
Kubernetes Virtual IP and Load-Balancer for both control plane and Kubernetes services

The idea behind `kube-vip` is a small self-contained Highly-Available option for all environments, especially:

- Bare-Metal
- Edge (arm / Raspberry PI)
- Virtualisation
- Pretty much anywhere else :)

**NOTE:** All documentation of both usage and architecture are now available at [https://kube-vip.io](https://kube-vip.io).

For upgrading an existing install in place (static Pod or DaemonSet), see the [upgrade guide](https://kube-vip.io/docs/upgrade/).

### Running kube-vip as a binary

[GitHub releases](https://github.com/kube-vip/kube-vip/releases) have
statically linked Linux binaries attached for `amd64`, `arm64`, `arm` (ARMv7),
`ppc64le` and `s390x`, named `kube-vip-linux-<arch>`, along with a
`kube-vip-checksums.txt` file holding their SHA-256 checksums. They let
kube-vip run directly on a host, for example as a systemd service next to
control plane components that are themselves deployed as binaries.

```shell
VERSION=vX.Y.Z # the release to install
ARCH=amd64
curl -fsSLO "https://github.com/kube-vip/kube-vip/releases/download/${VERSION}/kube-vip-linux-${ARCH}"
curl -fsSLO "https://github.com/kube-vip/kube-vip/releases/download/${VERSION}/kube-vip-checksums.txt"
sha256sum --check --ignore-missing kube-vip-checksums.txt
sudo install -m 0755 "kube-vip-linux-${ARCH}" /usr/local/bin/kube-vip
```

The binary accepts the same flags and environment variables as the container
image. Run it as root (or with `CAP_NET_ADMIN` and `CAP_NET_RAW`) so it can
manage addresses and send ARP. It reads `/etc/kubernetes/admin.conf` by
default, and `--autodetectcp` makes it reach the API server running on the same
host instead of the VIP it is about to bring up. To use a different kubeconfig,
set `--k8sConfigPath` and put the local API server address in the
`kubernetes_addr` environment variable. A minimal control plane VIP in ARP mode
as a systemd unit:

```ini
[Unit]
Description=kube-vip
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/kube-vip manager --interface eth0 --address 192.168.0.40 \
    --controlplane --arp --leaderElection --autodetectcp
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

`make release-binaries` builds the same set of binaries locally into `dist/`.

### Verifying release artifacts

Container images and release binaries are signed keylessly with
[cosign](https://docs.sigstore.dev/cosign/overview/) using the GitHub Actions
OIDC identity. Release images and binaries also have a signed SLSA build
provenance attestation available through the GitHub attestation API, and each
image has a signed CycloneDX SBOM attestation in the registry.

Signatures and SBOM attestations are attached in both the current Sigstore
bundle format and the legacy cosign format, so cosign v3 (recommended) and
cosign v2 can verify the same artifacts.

For images published by a release (`ghcr.io/kube-vip/kube-vip:<version>`,
`plndr/kube-vip:<version>`, and the corresponding `kube-vip-iptables` images):

```shell
VERSION=vX.Y.Z # the release to verify

cosign verify "ghcr.io/kube-vip/kube-vip:${VERSION}" \
  --certificate-identity-regexp '^https://github\.com/kube-vip/kube-vip/\.github/workflows/release\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

cosign verify-attestation --type cyclonedx "ghcr.io/kube-vip/kube-vip:${VERSION}" \
  --certificate-identity-regexp '^https://github\.com/kube-vip/kube-vip/\.github/workflows/release\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

gh attestation verify "oci://ghcr.io/kube-vip/kube-vip:${VERSION}" --repo kube-vip/kube-vip
```

The same cosign commands work with the Docker Hub names (`plndr/kube-vip`,
`plndr/kube-vip-iptables`).

Nightly images built from `main` are signed by `.github/workflows/main.yaml`;
use the same commands with the `main` tag and this identity:

```shell
cosign verify ghcr.io/kube-vip/kube-vip:main \
  --certificate-identity https://github.com/kube-vip/kube-vip/.github/workflows/main.yaml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Release binaries are covered by `kube-vip-checksums.txt`. Verify the checksums
signature before trusting the hashes, then check the provenance of an
individual binary:

```shell
cosign verify-blob kube-vip-checksums.txt \
  --bundle kube-vip-checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/kube-vip/kube-vip/\.github/workflows/release-binaries\.yaml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing kube-vip-checksums.txt

gh attestation verify kube-vip-linux-amd64 --repo kube-vip/kube-vip
```

### Runtime configuration files

The `manager` and `service` commands accept strict YAML or JSON configuration
with `--config-file PATH` or the `config_file` environment variable. Values are
resolved in this order: explicitly supplied CLI flags, environment variables,
configuration file, then command defaults. An explicitly supplied `false`, `0`,
or empty string overrides lower-priority sources; an omitted value does not.
`--config-file` takes precedence over `config_file` when both are set.

Configuration keys use the documented external names (for example `enableARP`,
`dnsDualStackMode`, and `bgpConfig.routerID`). Unknown keys are rejected. Runtime-
derived `isDualStack` and `requireDualStack`, the bootstrap `configFile` key, and
generator-only `loadBalancers`, `bgpPeers`, `singleNode`, `startAsLeader`, and
`addPeersAsBackends` keys are intentionally not accepted in runtime files.
Use `bgpConfig.peers` for BGP peers; the legacy single `bgpPeerConfig` form is
also accepted and normalized into the runtime peer list.

### DHCPv4 Service lease safety

DHCPv4 lease deadlines are measured from receipt of the ACK, not from delivery
of an address to a consumer. `Stop` is latched and cancels acquisition, retries
and renewal; await the client's `Start` completion before relinquishing its
interface. A renewal or rebind ACK for another address is rejected without
refreshing the bound address's expiry. Fresh DORA acquisition after a NAK can
still allocate and publish a new address.

Service supervision uses actual client completion to cancel its captured
Service generation, including when status publication is stalled. Address
changes retire old local workers before API publication and use canonical
Service generation replacement after matching status/watch feedback. This can
interrupt traffic and release/re-request DHCP; it is not an in-place or
zero-downtime address swap.

Instance-managed DHCP macvlans carry the native alias
`kube-vip:dhcp:v1:<full Service UID>:<canonical MAC>`. Adoption requires matching
UID, type, parent, mode, MAC and marker; unmarked or foreign same-name links are
refused before link changes or DHCP. An omitted MAC annotation can reuse the
validated persisted MAC. Explicit MACs must be six-byte, nonzero unicast values;
Service UIDs must contain 8–128 ASCII letters, digits, `-`, `_` or `.`. Selected
DHCP interface names must be 1–15 bytes, not `.` or `..`, with no `/`, `:`,
whitespace or NUL. With a recorded DHCPv4 completion signal, attachment cleanup
signals `Stop` and waits for actual `Start` completion before ownership transfer
or link deletion. There is no timeout-delete fallback: the attachment is retained
while completion is pending. Cleanup still validates captured native identity;
client transports check the original index/name/MAC/alias rather than reopening
a reused name. These checks do not
provide atomic validate-and-operate or authenticate concurrent privileged
identity forging/index reuse.

### Isolated DHCPv4 lifecycle checks

The [client](pkg/vip/dhcpv4_backoff_test.go) and
[Service](pkg/services/dhcpv4_lifecycle_linux_test.go) integration checks exercise
real public lifecycle APIs, native interfaces and packets with synthetic
external DHCP/Kubernetes peers, not mocked product methods. Compile on Linux or
cross-compile the public test binaries:

```shell
test_dir="$(mktemp -d /tmp/kube-vip-dhcp-tests.XXXXXX)"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -mod=readonly -o "$test_dir/vip.test" ./pkg/vip
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -mod=readonly -o "$test_dir/services.test" ./pkg/services
```

Run them only in a dedicated network-none Linux namespace initially containing
only loopback. Use a read-only root filesystem, mount only the public binary
read-only, drop all capabilities and add only `NET_ADMIN`, `NET_RAW` and
`NET_BIND_SERVICE`. Inside that namespace set `KUBE_VIP_ISOLATED_DHCP_TEST=1` and
run `vip.test -test.run '^TestDHCPv4' -test.v` or `services.test -test.v`.
Never use host networking or add `SYS_ADMIN` to remove namespace-related skips.

These traces are not a complete shared-address, VLAN, IPv6, BGP/election,
late-event, rollback or live-deployment certification. Preserve explicit skips
and unexercised cases as coverage gaps rather than treating a passing suite as
proof of every lease-ownership mode.

### Scoped DHCPv6 lifecycle checks

Each DHCPv6 client accepts one `Start`. `Stop` before `Start` releases its reserved
manager reference; during `Start`, it cancels only that owner's operation. The
reference is retained until actual `Start` completion, after active-lease RELEASE
cleanup has been attempted. `Stop` is a signal, not a completion join: await
`Start` before retiring its interface. `Instance.CleanupLinkAttachments` stops
both families and waits for their actual `Start` completion before transferring
or deleting attachments, including an owned VLAN used as the IPv6 transport
parent. Its IPv6 signal closes only after `Start` and its deferred manager-reference
cleanup return; no timer or forced socket-close override is added. A failed remote
RELEASE is still an error, not a guarantee that a server has released the lease.

Managed IPv6 sockets bind the current native interface index rather than a
cached interface-name zone. The constructor verifies the actual bound native
scope and closes its owned socket on failure. This is not atomic protection
against concurrent privileged interface replacement or index reuse.

The [Linux client checks](pkg/vip/dhcpv6_lifecycle_linux_test.go) exercise real
public acquisition, matching active-lease RELEASE and same-name interface
reconstruction, including consecutive cases in one process. They use the same
binary compilation and isolated three-capability network boundary above. Select
`TestDHCPv6PublicStartAcquiresLease`, `TestDHCPv6StopReleasesActiveLease` and
`TestDHCPv6PublicConstructorRecreatesInterfaceWithSameName`. The additional
`TestDHCPv6StopPreservesOtherActiveClientOnSharedParent` covers two active clients
with distinct explicit hostname-derived DUIDs and leases: stopping one releases
only its lease, while the other renews past its original expiry before its own
matching RELEASE and completed `Start`. Both callers and native interfaces stay
live. The synthetic peer receives raw IPv6 packets and replies through native
UDP6; a separate raw socket observes the wire. Product client methods are not
mocked. The decoder self-check `TestDHCPv6WireObserverRecognizesRelease` needs no
added capabilities.

The Service fixture also includes
`TestDHCPDualStackServiceWatchDeleteStopsClientsAndReleasesLeases`: a real external
Kubernetes Deleted event retires one no-VLAN dual-stack Service. Both leases,
kernel VIPs and statuses are established first; independent matching IPv4/IPv6
wire RELEASEs, actual client Start completion readback, IPv6 socket closure,
indexed attachment/instance removal and endpoint-watch cancellation are checked
with the root watcher, caller and original parent/peer live before T1.

The public Instance checks also create an owned VLAN through a static Service
constructor and transfer it to a real DHCPv6 Service using public cleanup.
Normal Stop and immediate Cleanup traces check the original lease's matching
SOLICIT/RELEASE wire/peer sequence, actual Start worker completion, indexed
VLAN/macvlan deletion and live original base/peer/caller before T1. Those responsive
8-second cases remain unchanged. The separate
`TestDHCPv6InstanceDelayedStopReleasesLeaseBeforeOwnedVLANRemoval` and
`TestDHCPv6InstanceDelayedCleanupWaitsBeforeOwnedVLANRemoval` use a 16-second
synthetic lease and withhold only the first terminal SOLICIT's ADVERTISE. The
normal Stop control observes the library's actual unchanged 5-second retry and
matching RELEASE with the parent retained. Cleanup during the independently
observed hold must complete actual Start before native indexed VLAN retirement;
raw/peer identity, actual worker, base/peer/caller and pre-T1 controls remain
required. Only the new fixture window changes; existing cases and all product,
library and runtime timers are unchanged.

The standalone IPv6-only watched-deletion case uses only an IPv6 Service family,
client and endpoint, with no IPv4 client, completion wait or DHCP wire traffic.
It checks the real lease, kernel VIP/status, raw/peer RELEASE correlation, actual
IPv6 Start completion/socket closure and owned attachment/instance removal with
the endpoint watch cancelled and original parent/peer/root watcher/caller live
before T1.

These checks do not certify unannotated/shared-identity cases, other peer-delay
or late-event schedules, concurrent VLAN replacement, rollback, all ownership
modes or live deployment. Keep those gaps and historical skips explicit; unit
reference-count checks alone are not shared-wire proof. The implemented actual
Start join and the bounded held-peer trace are not a general atomic ownership
or interface-index-reuse guarantee.

## Features

Kube-Vip was originally created to provide a HA solution for the Kubernetes control plane, over time it has evolved to incorporate that same functionality into Kubernetes service type [load-balancers](https://kubernetes.io/docs/concepts/services-networking/service/#loadbalancer).

- VIP addresses can be both IPv4 or IPv6
- Control Plane with ARP (Layer 2) or BGP (Layer 3)
- Control Plane using either [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection) or [raft](https://en.wikipedia.org/wiki/Raft_(computer_science))
- Control Plane HA with kubeadm (static Pods)
- Control Plane HA with K3s/and others (daemonsets)
- Service LoadBalancer using [leader election](https://godoc.org/k8s.io/client-go/tools/leaderelection) for ARP (Layer 2)
- Service LoadBalancer using multiple nodes with BGP
- Service LoadBalancer address pools per namespace or global
- Service LoadBalancer address via (existing network DHCP)
- Service LoadBalancer address exposure to gateway via UPNP
- Egress! Kube-vip will utilise a service loadbalancer as both the ingress and **egress** for a pod. 
- ... manifest generation, vendor API integrations and many more...

## Why?

The purpose of `kube-vip` is to simplify the building of HA Kubernetes clusters, which at this time can involve a few components and configurations that all need to be managed. This was blogged about in detail by [thebsdbox](https://twitter.com/thebsdbox/) here -> [https://thebsdbox.co.uk/2020/01/02/Designing-Building-HA-bare-metal-Kubernetes-cluster/#Networking-load-balancing](https://thebsdbox.co.uk/2020/01/02/Designing-Building-HA-bare-metal-Kubernetes-cluster/#Networking-load-balancing).

### Alternative HA Options

`kube-vip` provides both a floating or virtual IP address for your kubernetes cluster as well as load-balancing the incoming traffic to various control-plane replicas. At the current time to replicate this functionality a minimum of two pieces of tooling would be required:

**VIP**:
- [Keepalived](https://www.keepalived.org/)
- [UCARP](https://ucarp.wordpress.com/)
- Hardware Load-balancer (functionality differs per vendor)


**LoadBalancing**:
- [HAProxy](http://www.haproxy.org/)
- [Nginx](http://nginx.com)
- Hardware Load-balancer (functionality differs per vendor)

All of these would require a separate level of configuration and in some infrastructures multiple teams in order to implement. Also when considering the software components, they may require packaging into containers or if they’re pre-packaged then security and transparency may be an issue. Finally, in edge environments we may have limited room for hardware (no HW load-balancer) or packages solutions in the correct architectures might not exist (e.g. ARM). Luckily with `kube-vip` being written in GO, it’s small(ish) and easy to build for multiple architectures, with the added security benefit of being the only thing needed in the container.

## Troubleshooting and Feedback

### SELinux and IPVS kernel modules

When using IPVS load balancing on nodes with SELinux enforcing, kube-vip may be
blocked from requesting kernel modules from inside the container. Symptoms can
include the kube-vip pod entering `Error` or `CrashLoopBackOff`, logs that show
`ensure IPVS kernel modules are loaded`, or audit denials for `module_request`
from `container_t`.

Load the required IPVS modules on every node that can run kube-vip before
deploying it:

```shell
sudo modprobe ip_vs
sudo modprobe ip_vs_rr
```

To persist this across reboots, add the modules to a file such as
`/etc/modules-load.d/kube-vip-ipvs.conf`:

```text
ip_vs
ip_vs_rr
```

Preloading only the required modules is preferred to enabling the SELinux
`domain_kernel_load_modules` boolean for containers.

### Metrics and profiling

kube-vip runs two independent observability HTTP servers. Metrics are on by
default and profiling is off.

#### Metrics endpoint

Metrics are exposed on `:2112` by default, configurable with
`--prometheusHTTPServer` or `prometheus_server`. An empty value disables the
server.

Exposed metrics cover service reconciliation events and errors, leader election
transitions, BGP session state and build info (version, build, node).

#### pprof endpoints (debug only)

Profiling can be enabled with `--enablePprof` CLI flag or `enable_pprof` env variable, and listens on
`127.0.0.1:6060` by default. The address can be configured with `--pprofHTTPServer` or
`pprof_server`.

```yaml
# Kubernetes manifest
containers:
  - name: kube-vip
    env:
      - name: enable_pprof
        value: "true"
      - name: pprof_server
        value: "127.0.0.1:1234"
```

Profiles available under `/debug/pprof/`: `profile` (CPU), `heap`, `allocs`,
`goroutine`, `threadcreate`, `trace`, `cmdline` and `symbol`. 

### Gateway API `LoadBalancer` services with no endpoints

Some Gateway API controllers create `LoadBalancer` services that intentionally have no Endpoints/EndpointSlices backends.

If you want kube-vip to reconcile such a service, opt in with:

```yaml
metadata:
  annotations:
    kube-vip.io/allow-reconcile-without-endpoints: "true"
spec:
  type: LoadBalancer
  externalTrafficPolicy: Cluster
```

Scope:
- Works only with `externalTrafficPolicy: Cluster`
- No effect for `Local`
- Default endpoint-gated behavior remains unchanged for services without this annotation

Please raise issues on the GitHub repository and as mentioned check the documentation at [https://kube-vip.io](https://kube-vip.io/).

## Community Tools

- **[KubeStellar Console — Guided kube-vip Install](https://console.kubestellar.io/missions/install-kube-vip)** — A step-by-step guided installation experience for kube-vip with pre-flight checks, validation, troubleshooting, and rollback support.

## Contributing

Thanks for taking the time to join our community and start contributing! We welcome pull requests. Feel free to dig through the [issues](https://github.com/kube-vip/kube-vip/issues) and jump in.

:warning: This project has issue compiling on MacOS, please compile it on linux distribution

Additionally it is now relatively easy and quick to develop with [skaffold](https://skaffold.dev/), and the `skaffold.yaml` exists within the root folder of the git repository.

### Set up a kind development environment

1. `kind create cluster --config ./testing/kind.yaml`
2. `kubectl apply -f https://kube-vip.io/manifests/rbac.yaml`
3. Create a load balancer range `configMap` from the kind cluster
4. Apply the CCM manifest
5. Start skaffold with `skaffold dev`
6. Start developing and see changes applied in real-time.

## Star History

[![Star History Chart](https://star-history.dera.page/svg?repos=kube-vip/kube-vip&type=Date)](https://star-history.dera.page/#kube-vip/kube-vip&type=date)
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2Fkube-vip%2Fkube-vip.svg?type=shield)](https://app.fossa.com/projects/git%2Bgithub.com%2Fkube-vip%2Fkube-vip?ref=badge_shield)


## License
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2Fkube-vip%2Fkube-vip.svg?type=large)](https://app.fossa.com/projects/git%2Bgithub.com%2Fkube-vip%2Fkube-vip?ref=badge_large)
