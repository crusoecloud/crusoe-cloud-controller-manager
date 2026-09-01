# CCM VPC-Native Pod Routing via the Upstream Route Controller — LLD

RFC: CRUSOE-97212. Target repo: `github.com/crusoecloud/crusoe-cloud-controller-manager` (go.mod:1, Go 1.26, k8s libs v0.35.5).

**Revision 2026-08-31 (h) — this document.** The custom `crusoe-route-controller`
(controller/opTracker/reaper/state machine, revisions a-f) is **superseded**: the
design pivots to the upstream `k8s.io/cloud-provider` route controller
(`node-route-controller`, already registered in `app.DefaultInitFuncConstructors`
and vendored at v0.35.5), driven by a Crusoe `cloudprovider.Routes`
implementation over the existing `sdn` seam. The revision-(g) analysis (old §19)
is folded into the body; decisions taken:

1. **Pod CIDR source**: a CCM-local **write-once mirror controller** copies the
   CiliumNode v4 /24 → `node.spec.podCIDR` + `podCIDRs` (cilium stays
   cluster-pool). Path B from the analysis — keeps multi-reservation expansion.
2. **Taint**: the standard `node.kubernetes.io/network-unavailable`, driven by
   the `NodeNetworkUnavailable` condition the upstream controller sets; the KCM
   node-lifecycle controller manages the taint. All custom taint/label patching
   (`crusoe.ai/pods-unroutable`, op-id/ready-at labels) is deleted.
3. **Custom Prometheus metrics**: all dropped. Observability = upstream events +
   controller-manager metrics + klog.
4. Branch `CRUSOE-97212-upstream-route-controller`, fresh MR.

Earlier revisions (a-f) and the superseded custom-controller sections live in
git history (see §16). Sections kept from the previous design because they still
hold: the `sdn` seam (§4), configuration (§5), NIC resolution (§9), the real
gRPC client (§17).

## 1. Overview & Scope

In native routing mode, cilium (cluster-pool IPAM) allocates each node a pod /24
from a KM-reserved VPC prefix reservation and records it in
`CiliumNode.spec.ipam.podCIDRs`. The CCM must create one SDN **pod CIDR
allocation** per node — an atomic pair of (static route on the VPC logical
router + port_security widening on the node NIC) — and gate scheduling until the
allocation is ready.

The machinery is now the **upstream route controller**
(`k8s.io/cloud-provider/controllers/route/route_controller.go`, registered as
`node-route-controller`): it periodically diffs `node.Spec.PodCIDRs` against
`Routes.ListRoutes`, calls `CreateRoute`/`DeleteRoute`, and maintains the
`NodeNetworkUnavailable` condition. This repo supplies:

- **`routes.CloudRoutes`** — the `cloudprovider.Routes` implementation
  (ListRoutes/CreateRoute/DeleteRoute) over the existing `sdn` client seam and
  NIC resolution (§6).
- **`routes.PodCIDRMirror`** — a write-once controller copying the CiliumNode v4
  /24 into `node.spec.podCIDR`/`podCIDRs`, because cluster-pool IPAM never
  populates them and the upstream controller reads nothing else (§7).
- **Wiring** in `internal/cloud.go` so `Cloud.Routes()` returns the impl in
  native mode and `(nil, false)` otherwise (§8).

Two SDN services exist. The CCM calls only the first:

- `island.v2.region.PodCIDRAllocationManagement` — async create/delete (via
  `island.v2.component.Operation`) + synchronous lists. The entire SDN surface.
- `island.v2.region.VPCPrefixReservationManagement` — **KM-owned**. The
  reservation ids arrive pre-provisioned via
  `CRUSOE_VPC_PREFIX_RESERVATION_IDS`; the CCM's only read
  (`ListVPCPrefixReservations` on the seam, `internal/routes/sdn/client.go:53-56`)
  happens when more than one reservation is configured, to pick the reservation
  containing a node's cidr at create time.

**In scope:** the two components above, the `internal/cloud.go` wiring, deletion
of the custom controller machinery, the flags contract for addon-controller
(§10), README update.

**Out of scope:**

- Enabling the `CloudControllerManagerWatchBasedRoutesReconciliation` feature
  gate — Alpha, default **false** in v0.35.5
  (controller-manager `pkg/features/kube_features.go:52-53`). We run the
  periodic `wait.NonSlidingUntil` path (route_controller.go:193-197) at
  `--route-reconciliation-period` (default 10s).
- Allocation deletion on node departure beyond what the upstream delete loop
  does: `DeletePodCIDRAllocations` is still **Unimplemented server-side**, so
  `DeleteRoute` surfaces that error until the server lands it (§6.3).
- IPv6, multiple pod prefixes, more than one routed podCIDR per node.
- v1/v2 cluster detection — `CRUSOE_ROUTING_MODE` is the only gate.

## 2. Requirements Summary

- Per-node happy path: cilium writes `CiliumNode.spec.ipam.podCIDRs` → mirror
  copies the v4 /24 to `node.spec.podCIDR`+`podCIDRs` (write-once) → next
  upstream pass sees the node with a podCIDR and no matching route →
  `CreateRoute` (synchronous: create allocation + poll op to terminal) →
  upstream sets `NodeNetworkUnavailable=False` → KCM removes the standard
  `node.kubernetes.io/network-unavailable` taint the kubelet registered with.
- Orphan GC: an allocation whose NIC maps to no live node is returned from
  `ListRoutes` with `TargetNode: ""`; the upstream pass deletes it (route
  controller's routeMap skips empty TargetNode when indexing —
  route_controller.go:286-288 — so `shouldDeleteRoute` fires). Upstream's
  `isResponsibleForRoute` containment check is vacuous by design
  (`--cluster-cidr=0.0.0.0/0`, §10): ownership is enforced by `ListRoutes`
  returning only our configured reservations' allocations.
- Fail-closed: `ListRoutes` returns an error (aborting the whole pass,
  route_controller.go:249-253) if any live node's NIC cannot be resolved — a
  silently dropped node would make its allocation look orphaned.
- Level-triggered, idempotent, no durable CCM-side state: everything is
  recomputed from Node objects + SDN List APIs each pass. Leader election comes
  free from the cloud-provider app (`--leader-elect=true`).

## 3. Architecture

```
                 kubelet --register-with-taints=
                 node.kubernetes.io/network-unavailable=:NoSchedule   (cross-repo ask)
                                        │
CiliumNode (cilium cluster-pool)        ▼
  spec.ipam.podCIDRs ──► PodCIDRMirror (this repo, §7) ──► node.spec.podCIDR/podCIDRs
                            write-once patch                        │
                                                                    ▼
                          upstream node-route-controller (k8s.io/cloud-provider)
                          every --route-reconciliation-period (10s):
                            ListRoutes ──diff──► CreateRoute / DeleteRoute
                            └► NodeNetworkUnavailable condition on Node
                                        │                        │
                                        ▼                        ▼
                          CloudRoutes (this repo, §6)    KCM node-lifecycle ctrl
                            sdn seam + NIC resolution      condition ⇒ taint add/remove
                                        │
                                        ▼
                          island.v2.region.PodCIDRAllocationManagement (gRPC, §17)
```

Activation chain, all required (verified against
cloud-provider@v0.35.5 `app/core.go:102-136`):

1. `--configure-cloud-routes` — **defaults true** (options/kubecloudshared.go:67).
2. `cloud.Routes()` returns `(impl, true)` — only in native mode (§8).
3. `--cluster-cidr` parses (`processCIDRs`, core.go:116-129; empty/absent is a
   parse error, and `routecontroller.New` additionally `klog.Fatal`s on zero
   CIDRs). `--allocate-node-cidrs` is **not** checked in the CCM path.

**Trap (load-bearing):** because `--configure-cloud-routes` defaults true, the
moment `Routes()` returns non-nil on a cluster without `--cluster-cidr`, CCM
startup dies. Therefore `Routes()` must return `(nil, false)` unless the native
config loads successfully (§8), and addon-controller must render
`--cluster-cidr` together with the native env (§10).

## 4. `sdn` Package — Interface & Types (unchanged)

`internal/routes/sdn/` is kept exactly as shipped (types.go, client.go, fake.go,
fake_log.go, grpc.go, mock/, tests). It mirrors the merged proto
(`island.v2.region.PodCIDRAllocationManagement`, schemas MR 4075). Summary of
the seam (full contract in the package's doc comments):

```go
// internal/routes/sdn/client.go
type PodCIDRAllocationClient interface {
	// Async: returned Operation starts IN_PROGRESS. Idempotent: an identical
	// request never creates a second allocation (never ALREADY_EXISTS).
	CreatePodCIDRAllocations(ctx context.Context, req CreatePodCIDRAllocationsRequest) (*Operation, error)
	// Async, intent-based (absent ids skipped as success). Sole caller is now
	// CloudRoutes.DeleteRoute (§6.3). Server-side still Unimplemented.
	DeletePodCIDRAllocations(ctx context.Context, req DeletePodCIDRAllocationsRequest) (*Operation, error)
	ListPodCIDRAllocations(ctx context.Context, q ListPodCIDRAllocationsQuery) ([]PodCIDRAllocation, error)
	ListPodCIDRAllocationOperations(ctx context.Context, q ListPodCIDRAllocationOperationsQuery) ([]Operation, error)
	ListVPCPrefixReservations(ctx context.Context, ids []string) ([]VPCPrefixReservation, error)
}
```

Error taxonomy (sentinels in client.go, mapped reason-based by grpc.go, §17):
`ErrDestinationConflict` (FAILED_PRECONDITION + ErrorInfo reason
`DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE`; retryable — clears when the old
VM's delete lands), `ErrInvalidArgument` (permanent), `ErrNotFound`,
`ErrUnavailable` (retryable). A create failure that lands in the operation
result (OVN/DB) does **not** roll back the row — the retry adopts it via
List-before-create. `DeletePodCIDRAllocations` returns a wrapped
`UNIMPLEMENTED` until the server implements it.

The logging fake (`fake.go`) with its test knobs (`PendingPolls`, `FailNext`,
`ConflictCIDRs`) remains the primary test double and the runtime client when
`CRUSOE_SDN_ENDPOINT` is unset.

Only one doc-level change: the "REAPER USE ONLY" comment on
`DeletePodCIDRAllocations` is updated — the reaper is gone; the upstream delete
loop via `CloudRoutes.DeleteRoute` is the sole caller.

## 5. Configuration (`config.go`, trimmed)

Env contract is **unchanged** (addon-controller MR 57 renders it): native mode
sets exactly `CRUSOE_ROUTING_MODE=native`, `CRUSOE_VPC_ID`,
`CRUSOE_VPC_PREFIX_RESERVATION_IDS` (comma-separated, creation order) —
all-or-none, partial set fails startup (`ErrInconsistentConfig`). Project id
from `CRUSOE_PROJECT_ID`; location resolved fail-fast at startup from the
cluster object via `--cluster-name` (`internal/routes/location.go`, fatal on
failure, cfg immutable afterwards). SDN client selection unchanged
(`CRUSOE_SDN_ENDPOINT` absent ⇒ logging fake; set ⇒ gRPC, mTLS iff the
`CRUSOE_SDN_CERT_FILE`/`_KEY_FILE`/`_CA_FILE` trio is set;
`ErrInconsistentSDNConfig` on partial trio or SDN vars in overlay mode).

Trimmed from `Config`: `PollInterval`, `ReaperInterval`, `ReaperGrace`,
`Workers` (all served the deleted custom controller). The create-poll cadence
becomes package consts (§6.2):

```go
const (
	createPollInterval = 5 * time.Second
	createPollTimeout  = 5 * time.Minute // CreateRoute's self-imposed deadline (§6.2)
)
```

## 6. `cloudprovider.Routes` Implementation (`internal/routes/routes.go`, new)

Single consumer: the upstream route controller
(`cloud-provider@v0.35.5/cloud.go:246-257` interface;
`Route{Name, TargetNode, DestinationCIDR, Blackhole, ...}` cloud.go:226-243).
The upstream controller serializes passes (one `reconcileNodeRoutes` at a time)
but fans Create/Delete out on goroutines under a 200-slot semaphore
(`maxConcurrentRouteOperations`, route_controller.go:56, :306) — every method
must be goroutine-safe.

```go
type CloudRoutes struct {
	cfg        *Config
	sdn        sdn.PodCIDRAllocationClient
	apiClient  client.APIClient
	nodeLister v1lister.NodeLister // injected via SetNodeLister before controllers start (§8)

	mu        sync.Mutex
	nicByNode map[string]cachedNIC // nodeName → {instanceID, nicID}; §9
}

func NewCloudRoutes(cfg *Config, sdnClient sdn.PodCIDRAllocationClient, apiClient client.APIClient) *CloudRoutes
func (r *CloudRoutes) SetNodeLister(l v1lister.NodeLister)

var _ cloudprovider.Routes = (*CloudRoutes)(nil)
```

The `clusterName` parameter on all three methods is ignored — scoping is by
reservation ids (List) and `--cluster-cidr` containment (upstream's
`isResponsibleForRoute`, route_controller.go:549).

### 6.1 `ListRoutes(ctx, clusterName) ([]*cloudprovider.Route, error)`

1. List all nodes from `nodeLister`. Build `nicID → nodeName` by resolving
   every live node's NIC (§9 — cached; **steady-state zero API calls**).
   **Fail-closed:** any resolution failure fails the whole call — upstream
   aborts the pass on a List error (route_controller.go:250-253), which is
   exactly right: a node missing from the map would make its established
   allocation look orphaned. (The old reaper's fail-closed principle, grace
   period and 50-delete cap are replaced by this rule + the cache-sync gate +
   `--cluster-cidr` scoping; see §12 for the residual risk.)
2. `ListPodCIDRAllocations(VPCPrefixReservationIDs: cfg.VPCPrefixReservationIDs)`
   — already scoped to our reservations, one RPC.
3. Map each allocation → `&cloudprovider.Route{Name: alloc.ID,
   TargetNode: types.NodeName(nicToNode[alloc.NetworkInterfaceID]),
   DestinationCIDR: alloc.DestinationCIDR}`. An allocation whose NIC maps to no
   live node gets `TargetNode: ""` → the upstream pass **deletes** it. That is
   the desired orphan GC (leaked allocations from `kubectl delete node` while
   the VM lived on, etc.).

`Route.Name` carries the allocation id; `DeleteRoute` receives routes "as
returned by ListRoutes" (cloud.go:255-256 contract), so no re-lookup is needed.

### 6.2 `CreateRoute(ctx, clusterName, nameHint, route) error`

Synchronous contract: upstream calls it with the root ctx and **no timeout**
(the goroutine holds a semaphore slot and the pass `wg.Wait`s —
route_controller.go:421-431, :459). The impl brings its own deadline. A slow
create stalls only the current pass; passes are serialized so there is no
pile-up.

1. `node, err := nodeLister.Get(string(route.TargetNode))` — upstream built the
   route from this same lister, so NotFound is a transient race → error (retry
   next pass).
2. `nicID := resolveNIC(node)` (§9). Error → error.
3. **List-before-create / adopt** (old C7 semantics, relocated):
   `ListPodCIDRAllocations(DestinationCIDR: route.DestinationCIDR,
   VPCPrefixReservationIDs: cfg.VPCPrefixReservationIDs)`:
   - found with **our** NIC → adopt, return nil (covers crash-between-create-
     and-poll, op-retention gaps, and FAILED creates whose row survived).
   - found with a **different** NIC → return the wrapped
     `sdn.ErrDestinationConflict`: upstream emits the `FailedToCreateRoute`
     event (route_controller.go:433-439) and retries next pass. The /24-reuse
     race self-resolves when the old VM's delete lands; once server-side Delete
     works, the orphan-GC path (§6.1) is the backstop.
   - absent → continue.
4. Pick the reservation by cidr containment — `reservationForCIDR` relocated
   from the old state machine (`internal/routes/reconcile.go:289-326` on the
   pre-rewrite tree): single configured id → returned without lookup; several →
   `ListVPCPrefixReservations` + `netip` containment.
5. `CreatePodCIDRAllocations` (1 spec, context `{ProjectID, VPCID, Location}`).
   Synchronous error → return it (conflict handled as in step 3).
6. **Poll to terminal** with our own deadline:
   `wait.PollUntilContextTimeout(ctx, createPollInterval, createPollTimeout,
   true, ...)` over
   `ListPodCIDRAllocationOperations(OperationIDs: [op.OperationID])`:
   - `SUCCEEDED` → return nil (upstream then sets the node's condition).
   - `FAILED` → return an error carrying `op.Error`; the surviving row is
     adopted by step 3 on the next pass.
   - op not returned (retention gap) → keep polling until deadline.
   - deadline → return error; the op continues server-side and the next pass
     adopts via step 3 (or re-creates — contractually idempotent).

No workqueue, no opTracker, no durable op-id labels: the poll happens inline,
and crash/failover recovery is simply "next pass, step 3".

### 6.3 `DeleteRoute(ctx, clusterName, route) error`

```go
op, err := r.sdn.DeletePodCIDRAllocations(ctx, sdn.DeletePodCIDRAllocationsRequest{
	IDs:     []string{route.Name}, // allocation id from ListRoutes (§6.1)
	Context: r.allocationContext(),
})
```

- Return the error as-is. **The server is the kill switch**:
  `DeletePodCIDRAllocations` is still Unimplemented server-side, so every
  delete attempt fails loudly (upstream logs it each pass,
  route_controller.go:373-378 — ~10s klog noise, no state change, safe). No
  in-code kill-switch const remains; when the server lands the RPC, deletes
  simply start working.
- No op polling: intent-based semantics make a repeated delete of an absent id
  a success, and the next pass's `ListRoutes` verifies absence. (Rows pending a
  delete op are still listed until it resolves → at most one redundant,
  idempotent retry.)

## 7. PodCIDR Mirror Controller (`internal/routes/mirror.go`, new)

Why: the upstream controller reads **only** `node.Spec.PodCIDRs`
(route_controller.go:330-333, :393-396); cilium cluster-pool IPAM writes only
`CiliumNode.spec.ipam.podCIDRs`. The mirror is the write-once bridge; cilium
stays cluster-pool (which is what preserves multi-reservation pod-range
expansion — KCM's RangeAllocator takes exactly one v4 range and was rejected
for that reason in the (g) analysis).

**Registration:** the exact existing extra-controller mechanism — the
`register.go` wrapper (`internal/routes/register.go:39-48`) and the `main.go`
map entry (`cmd/crusoe-cloud-controller-manager/main.go:44-51`), with the key
renamed `"crusoe-route-controller"` → `"crusoe-podcidr-mirror"`. The init func
returns `(nil, false, nil)` when `CRUSOE_ROUTING_MODE != native` (same gate as
today, register.go:62-67). `register.go` is otherwise rewritten: SDN client,
Crusoe API client and location wiring move to `internal/cloud.go` (§8); the
mirror needs only the kube client, the dynamic CiliumNode informer (existing
GVR, register.go:33-35) and the shared Node informer.

**Behavior (write-once):**

```go
type PodCIDRMirror struct {
	kubeClient       clientset.Interface
	ciliumNodeLister cache.GenericLister
	nodeLister       v1lister.NodeLister
	// + synced funcs, typed workqueue [string] keyed by node name, 1 worker
}
```

- Handlers: CiliumNode Add/Update → enqueue(name); Node Add → enqueue(name)
  (covers CiliumNode-before-Node ordering). No delete handlers, no Node Update
  handler (the field is immutable once set). Informer resync (30m) is the
  level-triggered backstop.
- Reconcile(name):
  1. `node := nodeLister.Get(name)`; NotFound → done.
  2. `node.Spec.PodCIDR != ""` → done. **Write-once**: `ValidateNodeUpdate`
     allows unset→set and rejects any change afterwards
     (k8s.io/kubernetes `pkg/apis/core/validation/validation.go:7261-7273`), so
     there is nothing to reconcile after the first write — and no flap risk.
  3. Get CiliumNode from the lister, convert via `ciliumNodeFromUnstructured`
     (§ below); pick the **first v4** prefix from `spec.ipam.podCIDRs`
     (`netip.ParsePrefix` + `Addr().Is4()`); none yet → done (the CiliumNode
     Update event re-drives).
  4. One strategic-merge patch setting **both** fields to the same single v4
     cidr — they must be equal and one-element
     (`{"spec":{"podCIDR":"<cidr>","podCIDRs":["<cidr>"]}}`).
  5. Error → rate-limited requeue. `klog.InfoS` on the write; no events.

**CiliumNode projection shrinks** (`ciliumnode.go`): the mirror needs only
`Name` + `PodCIDRs`; `UID`/`ResourceVersion`/`DeletionTimestamp` (used by the
deleted state machine) are dropped from the struct and conversion. Fixture test
(`testdata/ciliumnode.json`) stays.

**Safety of the mirror window:** an unmirrored node has empty
`node.Spec.PodCIDRs`, so the upstream pass skips it for creates; its allocation
(if any pre-exists) maps to a live node via NIC, so `TargetNode` is set but no
podCIDR action exists → upstream would delete it. See the migration note in
§12 — harmless while server-side Delete is Unimplemented, and the mirror must
be deployed before server Delete lands.

## 8. `internal/cloud.go` Wiring & Construction Order

The framework's call order (verified, cloud-provider@v0.35.5
`app/controllermanager.go:295-302`): `cloud.Initialize(clientBuilder, stopCh)` →
`SetInformers(sharedInformers)` (iff the cloud implements
`cloudprovider.InformerUser`) → each controller InitFunc (which is when
`startRouteController` calls `cloud.Routes()`, app/core.go:109). So everything
`Routes()` needs can be built in `Initialize` + `SetInformers`.

One gap: location resolution needs the `--cluster-name` flag value
(`completedConfig.ComponentConfig.KubeCloudShared.ClusterName`), which
`Initialize` does not receive. `doInitializer` in `main.go` (main.go:71-83)
**does** receive `*config.CompletedConfig` — it stashes the cluster name on the
Cloud before returning it:

```go
// main.go doInitializer, after InitCloudProvider:
if c, ok := cloud.(*cloudcontrollermanager.Cloud); ok {
	c.SetClusterName(cfg.ComponentConfig.KubeCloudShared.ClusterName)
}
```

```go
// internal/cloud.go
type Cloud struct {
	crusoeInstances *instances.Instances
	apiClient       client.APIClient   // now retained from newCloud (was local)
	clusterName     string             // set by doInitializer before Initialize runs
	routes          *routes.CloudRoutes // nil ⇒ overlay mode
}

func (c *Cloud) SetClusterName(name string)

func (c *Cloud) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	// ...existing informer warm-up (cloud.go:27-30) unchanged...
	cfg, err := routes.LoadConfigFromEnv()
	if err != nil { klog.Fatalf(...) }              // partial env = misrender, fail fast
	if cfg.RoutingMode != routes.RoutingModeNative { return } // overlay: c.routes stays nil
	// location, fail-fast (§5): resolveLocationFromCluster(ctx, c.apiClient,
	// cfg.ProjectID, c.clusterName); err → klog.Fatalf (crash-loop is the retry)
	// SDN client per §5/§17 (fake when endpoint unset); Close wired to stop:
	//   go func() { <-stop; closeSDN() }()
	c.routes = routes.NewCloudRoutes(cfg, sdnClient, c.apiClient)
}

func (c *Cloud) SetInformers(f informers.SharedInformerFactory) { // cloudprovider.InformerUser
	if c.routes != nil {
		c.routes.SetNodeLister(f.Core().V1().Nodes().Lister())
	}
}

func (c *Cloud) Routes() (cloudprovider.Routes, bool) { // replaces cloud.go:43-45
	return c.routes, c.routes != nil
}
```

Notes:

- `Initialize` has no error return; **`klog.Fatalf` is the fail-fast** for
  native-mode construction failures (same crash-loop-retry semantics the old
  InitFunc-error path had; precedented in upstream cloud providers).
- `SetInformers` runs before any controller starts
  (controllermanager.go:300-302), so the lister is always set before the first
  `ListRoutes`; the upstream controller additionally gates its first pass on
  node-cache sync (route_controller.go:183-185) against the **same** shared
  informer, so the lister is also synced.
- `buildSDNClient` and the location resolution move from `register.go` into
  this path (exported from the `routes` package as needed); `register.go`
  retains only the mirror wiring (§7). `buildAPIClient` duplication disappears:
  `newCloud` already builds the API client (cloud.go:63-76) — it is now stored
  on the struct instead of rebuilt.

## 9. NIC Resolution (`nic.go`, relocated)

Resolution logic is unchanged (`internal/routes/nic.go:37-57`): instance via
`ProviderID` → `SystemUUID` → `GetInstanceByName`
(nic.go:104-112 `instanceIDFromNode`), then the NIC whose
`Network == cfg.VPCID` (nic.go:116-131 `selectNICID`; no fallback pick — a miss
is a hard retryable error naming the VPC and NICs found). Metadata mismatch
warnings stay (nic.go:138-147).

Two mechanical changes:

- The methods move from the deleted `RouteController` onto `CloudRoutes`
  (the free functions `instanceIDFromNode`/`selectNICID` are already
  standalone).
- The cache moves from the old `state` map to `CloudRoutes.nicByNode`, now
  storing `{instanceID, nicID}`: on lookup, if `instanceIDFromNode(node)`
  differs from the cached `instanceID`, re-resolve (guards a node deleted and
  recreated under the same name with a new VM — the old design cleared state on
  CiliumNode delete; `CloudRoutes` has no delete signal, so the key check
  replaces it). Nodes not yet carrying a providerID/SystemUUID resolve by name
  and cache with empty `instanceID`, self-correcting once the providerID
  appears. Entries for departed nodes linger harmlessly (map is per-process,
  bounded by lifetime node-name cardinality).

Steady state: zero Crusoe API calls per pass — every live node's NIC is cached.

## 10. Flags Contract (addon-controller / clusterlet)

Rendered on **native clusters only**, alongside the existing env (§5):

| Flag | Value | Why |
|---|---|---|
| `--cluster-cidr` | **`0.0.0.0/0`, constant** | Required once `Routes()` is non-nil (empty is fatal — §3 trap). Max one v4 (two only as a dual-stack pair, core.go:121-129). It only scopes **deletions** (`isResponsibleForRoute`); ownership is already guaranteed by `ListRoutes` filtering to `CRUSOE_VPC_PREFIX_RESERVATION_IDS`, so the widest scope is safe and correct. Deliberately NOT a reservation supernet: pod ranges get added out-of-band (a second cilium cluster-pool CIDR after a mis-sized create), and a value that must be widened in lockstep is a drift bug waiting to happen. This flag must never need updating. |
| `--configure-cloud-routes` | leave default (`true`) | Overlay clusters are protected by `Routes() = (nil,false)`, which self-disables the controller with a log warning (core.go:109-113). |
| `--route-reconciliation-period` | leave default (`10s`) | Pass cadence. |
| `--allocate-node-cidrs` | do **not** set | Not consulted in the CCM route path; KCM CIDR allocation stays off (cilium cluster-pool owns allocation). |

Feature gates: leave `CloudControllerManagerWatchBasedRoutesReconciliation`
**off** (Alpha default-false in v0.35.5).

Unchanged env contract: `CRUSOE_ROUTING_MODE` / `CRUSOE_VPC_ID` /
`CRUSOE_VPC_PREFIX_RESERVATION_IDS` / `CRUSOE_SDN_*` (fake client when the
endpoint is absent stays, §5).

## 11. Condition, Taint & Bootstrap Window

- The upstream controller owns the `NodeNetworkUnavailable` condition
  (`updateNetworkingCondition`, route_controller.go:498: `False`/reason
  `RouteCreated` when all the node's routes exist, `True` otherwise).
- The **KCM node-lifecycle controller** (not this CCM) maps that condition to
  the standard `node.kubernetes.io/network-unavailable:NoSchedule` taint — add
  *and* remove. No taint code remains in this repo.
- **Bootstrap window**: before the first successful pass the condition is
  absent, so nothing prevents scheduling. The cross-repo ask (clusterlet)
  stands, with the **standard key** now:
  `kubelet --register-with-taints=node.kubernetes.io/network-unavailable=:NoSchedule`.
  The lifecycle controller lifts it once the route lands and the condition goes
  `False`.
- All custom taint/label machinery is deleted: `crusoe.ai/pods-unroutable`,
  `crusoe.ai/pod-cidr-allocation-op-id`, `crusoe.ai/pod-cidr-allocation-ready-at`
  (`controller.go` consts + `nodepatch.go`).

## 12. Errors & Edge Cases

| Edge | Handling |
|---|---|
| Node without `spec.podCIDR` (mirror lag) | Upstream skips it for creates; no route yet ⇒ nothing to delete. Mirror event re-drives within seconds. |
| NIC resolution fails for any live node | `ListRoutes` fails ⇒ whole pass aborted (fail-closed). Risk: one unresolvable node stalls route reconciliation cluster-wide until it resolves or the Node object is removed (the CCM's own node-lifecycle controller removes nodes whose VM is gone). Accepted — see §18. |
| Allocation whose NIC maps to no live node | `TargetNode: ""` ⇒ upstream deletes (orphan GC). Ownership scoping comes from `ListRoutes`' reservation-id filter (`--cluster-cidr` is `0.0.0.0/0`, §10). Inert until server-side Delete lands. |
| `DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE` (/24-reuse race) | `CreateRoute` returns the conflict error ⇒ `FailedToCreateRoute` event + retry next pass; condition stays `True`, taint stays on. Self-resolves when the old VM's delete lands; orphan GC is the backstop post-server-Delete. |
| Create op `FAILED` (OVN/DB) | Error from `CreateRoute`; row survives server-side; next pass adopts it via List-before-create. |
| Crash/leader failover mid-create | Op abandoned; next pass List-before-create adopts the row or re-creates (contractually idempotent, never a second allocation). |
| Poll deadline (5m) exceeded | Error; op continues server-side; next pass adopts. |
| `DeleteRoute` while server Unimplemented | Error surfaced + logged by upstream each pass; no state change; safe noise. |
| SDN unavailable | `ListRoutes` error aborts the pass; next tick retries. No backoff beyond the 10s period — acceptable, calls are cheap Lists. |
| Manual pod-range expansion (second cilium cluster-pool CIDR after a mis-sized create) | Runbook: KM creates a VPC prefix reservation for the new range → append its id to `CRUSOE_VPC_PREFIX_RESERVATION_IDS` → add the CIDR to the cilium config. `--cluster-cidr` (`0.0.0.0/0`) is untouched. New nodes then get new-range /24s; the mirror copies them as usual and `reservationForCIDR` picks the containing reservation (`reservation.go`, tested by `TestCreateRoute_MultiReservationContainment`). If the env append lags the cilium change, creates for new-range nodes fail loudly (`ErrNoReservationForCIDR` or server-side containment rejection) with `FailedToCreateRoute` events and the node stays tainted — fail-safe, self-heals when the env lands. Existing nodes keep their /24 (one routed /24 per node — k8s caps `node.spec.podCIDRs` at one per family; §1 out-of-scope). |
| Multiple CiliumNode podCIDRs | Mirror copies the first v4 only; one routed /24 per node (unchanged policy). |

**Migration note (ordering, load-bearing):** enabling this build on a cluster
with **pre-existing allocations** before the mirror has populated
`node.spec.podCIDR` makes those allocations look actionless (live `TargetNode`,
no podCIDR action) → the upstream pass issues deletes for them. Harmless today
(server-side Delete is Unimplemented — the delete fails), but the sequencing
requirement is: **the mirror must be deployed and synced before server-side
`DeletePodCIDRAllocations` lands.** Recorded as the rollout gate.

## 13. Observability

All custom Prometheus metrics are deleted (`metrics.go` and its registrations).
Remaining surface:

- **Events**: upstream's `FailedToCreateRoute` (Warning, on the Node) — the
  only event in the route path. The mirror emits none.
- **Metrics**: stock controller-manager metrics only
  (`ControllerStarted/Stopped{"route"}`, workqueue/client metrics).
- **Logs**: upstream route controller logs every create/delete/condition action
  at Info. Our code: `klog.InfoS`/`ErrorS` with keys `node`, `cidr`,
  `allocationID`, `operationID`, `nicID`; the fake SDN client logs every
  would-be RPC (§4); mirror logs each write-once patch.

Alerting that referenced the deleted metrics (conflict counter, desired/actual
gauges) has no replacement in this drop; sustained `FailedToCreateRoute` events
are the conflict signal.

## 14. RBAC & Deployment

**RBAC delta: none.** The previously specified least-privilege set already
covers everything the new architecture needs: `nodes` get/list/watch/patch
(mirror spec patch + upstream taints/labels), `nodes/status` patch (upstream
condition), `events` create/patch, `ciliumnodes` get/list/watch (mirror).
For reference (input to the addon-controller manifest, unchanged):

```yaml
- apiGroups: ["cilium.io"]
  resources: ["ciliumnodes"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: [""]
  resources: ["nodes/status"]
  verbs: ["patch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
```

Deployment env: unchanged (§5). New addon-controller ask: render
`--cluster-cidr` per §10 on native clusters. The current release manifest binds
`cluster-admin` (`releases/crusoe-cloud-controller-manager/v0.1.2.yaml:9-19`),
so nothing blocks rollout.

## 15. Testing Strategy

Runner: `make test` (`-race -cover`); lint: `make lint`. The **upstream route
controller is not re-tested** — it ships with its own suite; we test our
`cloudprovider.Routes` contract compliance and the mirror.

| Layer | Tooling | Coverage |
|---|---|---|
| `CloudRoutes` | fake sdn client (`sdn.NewLoggingFakeClient` + knobs) + gomock `APIClient` + node lister from `cache.NewIndexer` (no informers, no envtest) | ListRoutes: allocation→node mapping via NIC; unresolvable-NIC → whole call errors (fail-closed); allocation with unknown NIC → `TargetNode: ""`; steady-state zero API calls (mock call count with warm cache); cache re-resolve when node's instance id changes. CreateRoute: happy path (create + poll to SUCCEEDED via `PendingPolls`); adopt pre-seeded row (ours) without create; conflict (other NIC / `ConflictCIDRs`) → error; `FailNext` → error, then next call adopts the surviving row; poll timeout → error; multi-reservation containment pick. DeleteRoute: id passthrough (`route.Name`), Unimplemented error surfaced, no op polling. |
| `PodCIDRMirror` | `k8sfake.NewSimpleClientset` + `dynamicfake` listers; drive reconcile directly | write-once: patch only when `spec.podCIDR` unset (pre-set → zero actions); patch sets podCIDR == podCIDRs[0], single v4; v6-first CiliumNode → picks the v4; empty podCIDRs → no-op; Node absent → no-op; patch payload asserted via clientset action list |
| `sdn` package | existing tests, unchanged | fake contract, gRPC error mapping (grpc_test.go) |
| `config.go` | existing table-driven tests, minus dropped knobs | all-or-none env validation, SDN trio rules |
| `ciliumnode.go` | fixture test, shrunk | `testdata/ciliumnode.json` → Name + PodCIDRs |
| `nic.go` helpers | existing tests, relocated onto `CloudRoutes` | resolution order, `ErrNoNICInVPC`, mismatch warnings |

Deleted with their subjects: `controller_test.go`, `optracker_test.go`,
`reaper_test.go`, `reconcile_test.go`, `helpers_test.go`, and the old
register/state-machine cases in `register_test.go`.

## 16. Superseded Design (pointer)

Revisions a-g of this document specified a custom `crusoe-route-controller`:
informer/workqueue topology, a per-node create state machine (C1-C11) with
durable op-id/ready-at Node labels, a batched async-operation poller
(`opTracker`), a grace-period/capped reaper, a custom taint
(`crusoe.ai/pods-unroutable`), and five custom Prometheus metrics. All of it is
replaced by the upstream route controller + §6/§7 of this revision. The full
text lives in git history of this file (last full version: the commit preceding
revision (h) on branch `CRUSOE-97212-upstream-route-controller`; implementation
on the superseded branch `CRUSOE-97212-vpc-native-pod-routing`).

## 17. Real gRPC Client — unchanged (landed in revision f)

`internal/routes/sdn/grpc.go` over `gitlab.com/crusoeenergy/schemas/api/island/v2`
(v2.216.28) — the only file touching pb/grpc types. Dial follows the
kubernetes-manager `grpcutil` convention (`rpc.SetupMTLS` →
`NewClientConnFactory(tlsConfig).NewClientConn(endpoint)`; empty cert trio ⇒
plaintext). Error mapping is reason-based on `google.rpc.ErrorInfo` (domain
`region.island.v2`, reason `DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE` →
`ErrDestinationConflict`); `INVALID_ARGUMENT`/`NOT_FOUND`/`UNAVAILABLE`+`ABORTED`
map to their sentinels; `UNIMPLEMENTED` stays a plain wrapped error
(`DeletePodCIDRAllocations` not yet served — §6.3). `Operation.AllocationIDs`
from `BulkOperationMetadata.resource_ids`. Client selection per §5
(`CRUSOE_SDN_ENDPOINT` absent ⇒ logging fake). Building requires
`GOPRIVATE=gitlab.com/crusoeenergy/*`. The only change in this revision is the
call site: the conn's `Close` is wired to `Initialize`'s stop channel (§8)
instead of the deleted register.go context hook.

## 18. Open Questions / Risks

- **Fail-closed ListRoutes stall**: one live node with an unresolvable NIC
  blocks all route reconciliation (creates included) until it resolves or the
  Node object is deleted. Correctness was chosen over availability (the
  alternative orphan-deletes a healthy node's route). Mitigation if it bites:
  exclude nodes younger than a threshold from the fail-closed rule.
- **No grace period / delete cap anymore**: once server-side Delete lands, a
  desired-state bug in `ListRoutes` could mass-delete routes in one pass
  (upstream has no cap). The fail-closed rule is the defense; re-evaluate
  before the server Delete rollout (that review is the reinstatement point for
  a cap inside `DeleteRoute` if wanted).
- **Blocking CreateRoute under SDN degradation**: a pass stalls up to
  `createPollTimeout` (5m) on the slowest create (passes are serialized).
  Visible as slow node readiness; acceptable at seconds-scale op latency.
- **List pagination** unspecified; one `ListPodCIDRAllocations` call per pass is
  assumed to return the full reservation set. Revisit past a few thousand /24s.
- **Mirror-before-server-Delete rollout gate** (§12 migration note) must be
  tracked in the addon-controller/kubernetes-manager sequencing.
- **`--cluster-cidr` / reservation drift**: eliminated by contract —
  `--cluster-cidr` is the constant `0.0.0.0/0` (§10), so deletion scoping never
  drifts when pod ranges are added out-of-band. The single source of truth for
  ownership is `CRUSOE_VPC_PREFIX_RESERVATION_IDS` (env append is part of the
  expansion runbook, §12).
- **Condition-absent bootstrap window** (§11): unchanged residual; closed by
  the kubelet `--register-with-taints` cross-repo ask.

## Implementation Status

Branch: `CRUSOE-97212-upstream-route-controller`. Each commit compiles, passes
`make lint` and `make test` independently. Toolchain (local verification):
`GOROOT=$HOME/.gvm/gos/go1.26.2 PATH=$HOME/.gvm/gos/go1.26.2/bin:$PATH
GOPATH=$HOME/.gvm/pkgsets/go1.25.6/global GOTOOLCHAIN=go1.26.6
GOPRIVATE='gitlab.com/crusoeenergy/*'`.

| # | Commit | Contents | Status |
|---|--------|----------|--------|
| 1 | docs: pivot design to upstream route controller (revision h) | This document + the README VPC-native section (already in the working tree). | done: a1fd810 |
| 2 | `internal/routes`: CloudRoutes — `cloudprovider.Routes` over the sdn seam | New `routes.go` (§6: ListRoutes fail-closed mapping, synchronous CreateRoute with adopt + 5m poll, DeleteRoute passthrough; NIC cache per §9) + `routes_test.go` (§15 row 1). Make `resolveInstance`/`instanceByID` standalone funcs in `nic.go` (parameterized on `client.APIClient`+`Config`) so both the old controller (untouched, still compiling) and `CloudRoutes` use them; relocate `reservationForCIDR` to a standalone func (new `reservation.go`), old call site delegates. Exported and deliberately unwired — `Cloud.Routes()` still returns `(nil,false)`. | done: 843a884 |
| 3 | `internal/routes`: PodCIDRMirror controller | New `mirror.go` + `mirror_test.go` (§7, §15 row 2). Exported, unwired (registration happens in commit 4). Uses the existing `ciliumNodeFromUnstructured`/GVR. | done: bc24cc3 |
| 4 | the swap: wire `Cloud.Routes()`, register the mirror, delete the custom controller | `internal/cloud.go`: `apiClient`/`clusterName`/`routes` fields, `Initialize` native construction (fatal on failure, SDN Close on stop), `SetInformers`, `Routes()` (§8); `main.go`: `SetClusterName` in `doInitializer`, map key → `"crusoe-podcidr-mirror"`. `register.go` rewritten to start only the mirror (§7). DELETE: `controller.go`, `optracker.go`, `reaper.go`, `reconcile.go`, `reconcile_helpers.go`, `nodepatch.go`, `metrics.go`, `helpers_test.go` + their tests. Shrink `ciliumnode.go` projection to Name+PodCIDRs (+test). Trim `Config` knobs (§5) + config tests. Update the `DeletePodCIDRAllocations` seam comment (§4). | done: 7a99804 |
| 5 | chore: tidy + status flips | `go mod tidy` (drop deps orphaned by the deletions, e.g. metrics-only imports), remove stale lint excludes for deleted files, flip this table's statuses, capture deviations. | done (this commit) |

### Deviations from the plan (captured at implementation)

- **Commit 2 NIC refactor scope:** the plan named `resolveInstance`/`instanceByID`; `warnMetadataMismatch` was also made standalone (same reason — shared by the old controller and `CloudRoutes`). `RouteController.resolveNIC` was left delegating to the free funcs until commit 4 deleted it.
- **`ResolveLocationFromCluster` / `BuildSDNClient` exported:** §8 said SDN + location wiring move into `internal/cloud.go` "exported from the routes package as needed". Location resolution became exported (`ResolveLocationFromCluster`) and `buildSDNClient` became `routes.BuildSDNClient` in a new `wiring.go`; `cloud.go` (package `crusoe`) calls both. `buildAPIClient` was dropped — `newCloud`'s client is retained on the `Cloud` struct per §8.
- **`crusoe-route-controller` → `crusoe-podcidr-mirror` rename** also renamed `StartRouteControllerWrapper` → `StartPodCIDRMirrorWrapper` and collapsed `register_test.go` to the wrapper smoke test (the mode-gate cases duplicated `config_test.go` coverage of `LoadConfigFromEnv`).
- **PodCIDRMirror tests use an indexer-backed `GenericLister`, not `dynamicfake`** (§15 row 2 mentioned dynamicfake). An indexer lister drives `reconcile` directly with fewer moving parts; the mirror reads CiliumNodes only through `cache.GenericLister`, so the double is equivalent.
- **`go mod tidy` pruned nothing:** the imports the deleted files used (prometheus metrics helpers, etc.) are still referenced by vendored k8s libraries, so `go.mod`/`go.sum` were unchanged.
- **Stale `sdn` doc comments:** §4 pins the `sdn` package as unchanged except the `DeletePodCIDRAllocations` comment, so a few internal comments in `sdn/{types,grpc,client}.go` still mention `opTracker`/`reaper` (now deleted). Left as-is to honor that scope; only the `SeedAllocation` comment (which pointed at a deleted reaper test) was refreshed. Follow-up: scrub the remaining `sdn` comments when that package is next touched.
- **No `.golangci.yml` change:** the config had no per-file excludes for the deleted files, so there was nothing to remove.

Sequencing rationale: commits 2-3 only add exported, unwired code (old
controller keeps compiling and passing its tests); commit 4 is the single
atomic swap so no intermediate commit runs two route implementations or leaves
unexported dead code for the `unused` linter.
