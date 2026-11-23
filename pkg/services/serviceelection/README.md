# Service election lifecycle

This package coordinates the readiness of Kubernetes Services with a shared
leader-election runner. It does not own Kubernetes Service state or program the
network datapath. Those capabilities are supplied through `Dependencies`.

## State flow

One Service context repeatedly moves through the following lifecycle:

```text
waiting for endpoints
        |
        v
ready generation -> coordinator membership -> shared election campaign
        |                                      |
        |                                      v
        |                              leading / observing
        |                                      |
        |                                      v
        |                              datapath activated
        v                                      |
readiness lost or context cancelled -----------+
        |
        v
datapath deactivated -> membership withdrawn -> wait for next generation
```

A readiness generation prevents delayed work for an old endpoint set from
activating a Service. A member identifies one Service context and one readiness
generation. A coordinator groups all local members which use the same election
Lease. A campaign represents one local attempt to run or observe that shared
election.

Owning an election session only means that this local component is responsible
for running the Kubernetes or etcd election. It does not mean that the node is
the elected leader. An observer uses the result of a runner started by another
local component sharing the same Lease.

## Synchronization boundaries

Several synchronization mechanisms deliberately coexist:

* the Service event queue preserves watch-event order for a Namespace/Name;
* `services.ServiceLock` serializes effects for one immutable Service UID;
* Service-context identity rejects work belonging to a replaced lifecycle;
* readiness generations reject work for an obsolete endpoint set;
* Lease registration tokens prevent stale cleanup from deleting a replacement
  registration;
* `member.operationMutex` serializes activation and cleanup for one member;
* `coordinator.mutex` protects membership and campaign state.

The following ordering rules are invariants:

1. Never wait for readiness operations while holding a Service lock. Activation
   reserves readiness before it may acquire the Service lock, so doing so would
   deadlock endpoint-loss reconciliation.
2. Never call `Datapath.Activate` or `Datapath.Cleanup` while holding
   `coordinator.mutex`.
3. Do not execute callbacks or other potentially blocking external operations
   while holding `coordinator.mutex`.
4. Snapshot members under `coordinator.mutex`, then perform member operations
   after releasing it.
5. Cleanup must validate both Service-context identity and member identity so a
   stale generation cannot remove a replacement.

## Ownership

The readiness watcher owns joining and closing a member. The coordinator owns
campaign selection and member activation. The local election-session runner
owns finalizing its session. The caller of `DetachForContext` already owns
datapath cleanup; that method only removes the matching membership to avoid
reacquiring the Service lock through cleanup.

Activation responsibility within one campaign is split as follows:

* when a campaign starts leading, its leadership path activates every member in
  its activation snapshot;
* a member admitted to an already leading campaign is activated by its own
  admission (`admitToCampaign`), and only that member;
* a member whose activation failed is retried only by its own backoff retry;
  admitting another member or restarting the campaign does not re-attempt it;
* a scheduled campaign restart (`startCampaign`) activates nobody when it finds
  a campaign already running, because that campaign's snapshot covers every
  remaining member.

Admission and the leader context are both published under `coordinator.mutex`,
so a member is reached either by the snapshot or by its admission;
`markMemberActive` prevents it from being activated twice.
