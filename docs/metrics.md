# kube-vip Prometheus metrics

kube-vip exposes Prometheus metrics from every kube-vip process. The metrics
endpoint is enabled by default at `:2112/metrics` and can be configured with
the `--prometheusHTTPServer` flag, the `prometheusHTTPServer` configuration
field, or the `prometheus_server` environment variable. Set the address to an
empty string to disable the endpoint.

Scrape each kube-vip pod or node-local process separately. Most gauges describe
the state managed by that process, while counters and histograms describe work
observed by that process and reset when it restarts. Use `rate()` or `increase()`
for counters instead of comparing their raw values across restarts.

Metrics that depend on a particular kube-vip mode are only emitted after that
mode exercises the corresponding code path. Label values listed below are the
values currently emitted by kube-vip; new values may be added as supported
backends grow.

## Service and VIP lifecycle

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `kube_vip_active_services` | Gauge | `namespace` | Number of managed LoadBalancer services in the namespace. |
| `kube_vip_service_reconcile_errors_total` | Counter | `namespace`, `name`, `reason` | Service reconciliation failures. Current reasons include `invalid_config`, `service_context`, `delete_service`, and `new_instance`. |
| `kube_vip_service_reconcile_duration_seconds` | Histogram | `namespace` | End-to-end duration of service `AddOrModify` reconciliation. |

## Dataplane operations

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `kube_vip_vip_addresses` | Gauge | `interface`, `family` | VIP addresses currently held by an interface. `family` is `IPv4` or `IPv6`. |
| `kube_vip_vip_operations_total` | Counter | `op`, `result` | VIP address operations. Current operations are `add` and `delete`; results are `ok` or `error`. |
| `kube_vip_arp_advertisements_total` | Counter | `result` | Gratuitous ARP advertisements, labeled `ok` or `error`. |
| `kube_vip_ndp_advertisements_total` | Counter | `result` | Gratuitous NDP advertisements, labeled `ok` or `error`. |
| `kube_vip_route_operations_total` | Counter | `op`, `result` | Routing-table operations. Current operations are `add`, `delete`, `replace`, and `update`; results are `ok` or `error`. |
| `kube_vip_dns_resolutions_total` | Counter | `result` | DNS resolutions performed by the DNS-backed VIP updater, labeled `ok` or `error`. |
| `kube_vip_dns_ip_changes_total` | Counter | — | Number of DNS-backed VIP changes where the resolved address changed. |

The VIP address gauge is state-based: an address is counted once per
interface/family and is removed when kube-vip releases it. Operation counters
are attempt/outcome signals and should normally be queried with `rate()`.

Useful examples:

```promql
sum by (instance, interface, family) (kube_vip_vip_addresses)
sum by (op, result) (rate(kube_vip_vip_operations_total[5m]))
sum by (result) (rate(kube_vip_arp_advertisements_total[5m]))
sum by (result) (rate(kube_vip_ndp_advertisements_total[5m]))
```

## Service events and leader election

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `kube_vip_manager_all_services_events_total` | Counter | `type` | Events received by the service watcher. `type` is a Kubernetes watch event such as `ADDED`, `MODIFIED`, `DELETED`, `BOOKMARK`, or `ERROR`. |
| `kube_vip_leader_election_transitions_total` | Counter | `lease_name` | Number of observed transitions into leadership for a lease. A high rate can indicate instability. |
| `kube_vip_is_leader` | Gauge | `node`, `lease_name` | `1` when the labeled node currently holds the lease, otherwise `0`. |
| `kube_vip_watcher_loops` | Gauge | `kind` | Number of live watcher goroutines. Current kinds are `service`, `endpoint`, `lease`, and `node`. Endpoint and lease watchers are per service. |
| `kube_vip_election_loops` | Gauge | `type` | Number of live leader-election loops. Current types are `kubernetes` and `etcd`. |
| `kube_vip_service_election_loops` | Gauge | `namespace`, `name` | Live per-service leader-election restart loops on this node. A value greater than `1` indicates leaked loops. |
| `kube_vip_service_election_attempts_total` | Counter | `namespace`, `name` | Attempts made by a per-service leader-election restart loop. |
| `kube_vip_service_election_errors_total` | Counter | `namespace`, `name`, `reason` | Per-service election failures. The current failure reason is `no_lease`. |

The loop gauges are balanced at goroutine start and exit. They are useful for
detecting a leaked watcher or election loop after a service update, leadership
loss, or watcher restart. For example:

```promql
max by (namespace, name, instance) (kube_vip_service_election_loops) > 1
sum by (instance, kind) (kube_vip_watcher_loops)
rate(kube_vip_leader_election_transitions_total[5m])
```

## BGP session state

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `kube_vip_manager_bgp_session_info` | Gauge | `state`, `peer` | BGP session state for a peer. For each peer, the current GoBGP state has value `1` and the other state series have value `0`; `peer` is emitted as `address:179`. |

This metric is only useful when BGP is enabled. A session alert should select
the state that represents an established session in the deployed GoBGP
version, rather than assuming that an absent series means a failed session.

## Build information

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `kube_vip_build_info` | Gauge | `version`, `build`, `node` | Constant `1`, useful for identifying the kube-vip version and build running on each node. |

## Querying safely

Service labels are intentionally included for troubleshooting, but they can
create many time series in clusters with many short-lived services. Prefer
aggregation or recording rules for dashboards, and use `rate()`/`increase()`
for counters:

```promql
sum by (namespace) (kube_vip_active_services)
sum by (result) (rate(kube_vip_service_reconcile_errors_total[5m]))
histogram_quantile(0.99,
  sum by (le) (rate(kube_vip_service_reconcile_duration_seconds_bucket[5m])))
```
