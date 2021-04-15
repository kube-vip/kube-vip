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
