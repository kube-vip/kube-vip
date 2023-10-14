# Running etcd Tests
## Prerequisites:
* Docker

If you want to use an image that only exists in your local docker cache, use this env var (modify registry and tag accordingly):
```sh
export E2E_IMAGE_PATH=plndr/kube-vip:v0.6.2
```

If you want to preserve the etcd nodes after a test run, use the following:
```sh
export E2E_PRESERVE_CLUSTER=true
```

Note that you'll need to delete them before being able to run the test again, this is only for debugging. You can use `kind delete cluster` or just `docker rm` the containers.

Tu run the tests:
```sh
ginkgo -vv --tags=e2e testing/e2e/etcd

```

The E2E tests:
1. Start 3 kind nodes (using docker)
2. Load the local docker image into kind 
3. Init the etcd cluster and join all nodes
4. Verify the etcd API can be accessed through the VIP
   1. This proves leader election through etcd in kube-vip is working.
5. Removes the first node (which is probably the VIP leader)
4. Verify the etcd API can be accessed through the VIP

> Note: this has only been tested on Linux but it might work on Mac