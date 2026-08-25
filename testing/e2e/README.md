# Running End To End Tests
Prerequisites:
* Tests must be run on a Linux OS
* Docker installed with IPv6 enabled [how to enable IPv6](https://docs.docker.com/config/daemon/ipv6/)
  * You will need to restart your Docker engine after updating the config
* Target kube-vip Docker image exists locally. Either build the image locally
  with `make dockerx86Local` or `docker pull` the image from a registry.

Run the tests from the repo root:
```
make e2e-tests
```

Note: To preserve the test cluster after a test run, run the following:
```
make E2E_PRESERVE_CLUSTER=true e2e-tests
```

The E2E tests:
* Start a local kind cluster
* Load the local docker image into kind
* Test connectivity to the control plane using the VIP
* Kills the current leader
    * This causes leader election to occur
* Attempts to connect to the control plane using the VIP
    * The new leader will need send ndp advertisements before this can succeed within a timeout

## Prometheus metrics helpers

The E2E build also provides helpers for checking the metrics endpoint from
inside a kind node. `ScrapeMetrics` executes `curl` against
`http://127.0.0.1:2112/metrics` and parses the result into a label-aware sample
map. The helpers are compiled only with the `e2e` build tag and do not change
the normal test suite.

Use `MetricValue` when a label selector should match one series; its second
return value is the number of matching series. Use `SumMetric` or `MaxMetric`
when a selector intentionally matches multiple series. `CounterDelta` compares
two scrapes of one counter, while `EventuallyMetric` and
`ConsistentlyMetric` provide Gomega polling assertions. `MetricStable` checks
that a value remains unchanged across samples separated by a caller-selected
gap, which is useful for detecting leaked loops after a fault.

The metrics-enabled tests require kube-vip to expose port `2112` in the kind
node and require `curl` in the node image. Keep metric assertions focused on
stable behavior and use label selectors instead of depending on the ordering
of Prometheus samples.
