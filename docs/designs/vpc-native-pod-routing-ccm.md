# CCM Route Controller for CMK VPC-Native Pod Routing — LLD

RFC: CRUSOE-97212. Target repo: `github.com/crusoecloud/crusoe-cloud-controller-manager` (go.mod:1, Go 1.26, k8s libs v0.35.5).

**Revision 2026-08-28.** SDN contract FINALIZED: `island.v2.region.PodCIDRAllocationManagement` merged in `gitlab.com/crusoeenergy/schemas` MR 4075 (supersedes the tentative `VPCRouteManagement` shapes this doc previously carried). Config surface per addon-controller MR 57 (merged direction). Deletion ownership per kubernetes-manager MR 1314 ("deleting a VM already removes its allocations SDN-side; KM sweeps at cluster delete as a backstop").

**Revision 2026-08-28 (b).** Op-id / ready-at moved from Node annotations to Node **labels** (ready-at stored as Unix epoch seconds — RFC3339 is an illegal label value); `PodCIDRAllocationContext.project_id`/`location` are **derived from the node's instance** (same API call that resolves the NIC) instead of requiring new deployment env — the `CRUSOE_LOCATION` follow-up against addon-controller is withdrawn.

**Revision 2026-08-28 (c).** Location resolution no longer waits for the first node: the primary source is the **cluster object** — `ListClusters(project)` matched on the `--cluster-name` flag the deployment already passes (v0.1.2.yaml:69) → `KubernetesCluster.Location` — resolved once at controller startup (§5.1). Instance-derived location remains the fallback/cross-check.

## 1. Overview & Scope

In native routing mode, cilium (cluster-pool IPAM) allocates each node a pod /24 from a KM-reserved VPC prefix reservation and records it in `CiliumNode.spec.ipam.podCIDRs`. This controller creates one SDN **pod CIDR allocation** per node — an atomic pair of (static route on the VPC logical router + port_security widening on the node NIC) — and gates scheduling until the allocation is ready. Nodes register with the kubelet-applied taint `crusoe.ai/pods-unroutable=:NoSchedule`; the CCM only ever *removes* it.

Two SDN services exist. The CCM calls only the first:

- `island.v2.region.PodCIDRAllocationManagement` — async create/delete (via `island.v2.component.Operation`) + synchronous lists. This controller's entire SDN surface.
- `island.v2.region.VPCPrefixReservationManagement` — synchronous, **KM-owned**. The CCM never calls it; the reservation id arrives pre-provisioned via `CRUSOE_VPC_PREFIX_RESERVATION_ID`. Mentioned for context only.

**In scope (this drop):**

- New custom controller `crusoe-route-controller` registered via `app.DefaultInitFuncConstructors` (same mechanism as the node-lifecycle override, `cmd/crusoe-cloud-controller-manager/main.go:35-41`).
- Create-side reconcile state machine: allocation create, durable op-id tracking via **Node labels**, batched async-operation polling (`opTracker`), `NetworkUnavailable` condition, taint removal (last), periodic reaper.
- `PodCIDRAllocationClient` Go interface mirroring the **merged** proto (plain Go structs, **no grpc/protobuf deps**) plus a logging fake implementation with an in-memory allocation/operation table honoring the contract's intent-based semantics. The fake is what ships this drop.
- Metrics, events, unit tests against the fake and k8s fake clients.

**Out of scope (this drop):**

- Real gRPC/mTLS SDN client (env vars are defined but unused; see §14 for the swap plan — the generated clients already exist in `gitlab.com/crusoeenergy/schemas`).
- **Allocation deletion on node departure.** Per KM MR 1314, deleting a VM removes its allocations SDN-side and KM sweeps at cluster delete; the CCM does **nothing** on CiliumNode deletion (no finalizer, no delete path). `DeletePodCIDRAllocations` is retained in the client interface for **reaper use only** (§11).
- `cloudprovider.Routes` — permanently rejected. `Cloud.Routes()` stays `nil, false` (`internal/cloud.go:43-45`). The upstream route controller requires `--configure-cloud-routes` + cluster CIDR and skips nodes with empty `node.spec.podCIDRs`, which never populate in cluster-pool IPAM.
- Any v1/v2 cluster detection. `CRUSOE_ROUTING_MODE` is the only gate: `overlay` (default) ⇒ one log line, controller not started.
- IPv6, multiple pod prefixes, more than one routed podCIDR per node (only `podCIDRs[0]` is routed; extras warn).
- Batch creates >1 (the RPC is capped at 1 allocation per call for now; the `repeated` shape lets the cap rise without contract change — >1 today returns INVALID_ARGUMENT).

## 2. Requirements Summary

- Per-node happy path: CiliumNode gets podCIDRs → `CreatePodCIDRAllocations` (1 spec) → patch Node label `crusoe.ai/pod-cidr-allocation-op-id` **immediately** (durable before anything depends on it) → `opTracker` batch-polls until `SUCCEEDED` → set Node condition `NetworkUnavailable=False` → one final Node patch: set label `crusoe.ai/pod-cidr-allocation-ready-at` (Unix epoch seconds, CCM-observed completion time), clear op-id, remove taint **last**.
- No delete path: CiliumNode deletion only clears in-memory soft state. VM delete owns SDN-side allocation removal (KM MR 1314).
- The **/24-reuse race is expected**: node B can receive a /24 whose allocation still points at dead node A's NIC (k8s node delete precedes VM delete completion). Create fails `FAILED_PRECONDITION` with ErrorInfo reason `DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE` → Node event + conflict metric + requeue with backoff; node stays tainted; self-resolves when the VM delete lands. Sustained conflicts indicate a leaked allocation (alert; reaper cleans).
- Reaper is the **only** defense against leaked allocations (e.g. `kubectl delete node` while the VM lives on, or VM-delete flows that failed after NIC teardown): desired `{destination_cidr → network_interface_id}` from CiliumNodes vs actual from `ListPodCIDRAllocations(vpc_prefix_reservation_ids=[reservation])`; batch-delete orphans (≤50 ids/call, chunked) with a grace period against racing joins; enqueue missing via the normal reconcile queue; NIC-mismatch = delete-then-recreate.
- Level-triggered, idempotent. Durable state lives on the Node (labels) — after leader failover the new leader reads the op id off the Node and resumes polling instead of blindly re-creating (re-create is also safe: create is contractually idempotent, §4).
- Leader election comes free from the cloud-provider app framework (`--leader-elect=true` already set, `releases/crusoe-cloud-controller-manager/v0.1.2.yaml:67`).

## 3. Package Layout

New package `internal/routes/` with a leaf subpackage `internal/routes/sdn/`. Rationale: `sdn` isolates the proto-mirroring types and the `PodCIDRAllocationClient` seam so that when the real gRPC client lands it is one new file in `sdn/` and zero changes elsewhere; it also gives `mockgen` a clean single-interface source, matching the existing `internal/client` → `internal/client/mock` pattern (`internal/client/mock/client.go`). The package and controller keep the operator-facing name "route(s)" (what the feature does for pods); identifiers that mirror the SDN resource say "allocation" (§4, metrics/events §13).

```
internal/routes/
├── register.go          # StartRouteControllerWrapper + config fail-fast (mirrors internal/node/utils.go:20-30)
├── config.go            # Config struct, LoadConfigFromEnv, env var consts, project/location sourcing
├── controller.go        # RouteController struct, constructor, Run, workers, informer handlers
├── optracker.go         # opTracker: batched async-operation polling (§7.1)
├── reconcile.go         # per-node create state machine
├── reaper.go            # periodic desired-vs-actual sweep (owns all deletes)
├── ciliumnode.go        # local ciliumNode struct + unstructured conversion (read-only; no writes)
├── nodepatch.go         # condition update + combined taint/label Node patch helpers
├── nic.go               # network_interface_id resolution + cache; project/location derivation
├── metrics.go           # component-base metrics, registered via legacyregistry
├── testdata/
│   └── ciliumnode.json  # captured real CiliumNode object (fixture for conversion tests)
├── *_test.go
└── sdn/
    ├── types.go         # PodCIDRAllocation, Operation, request/query structs (plain Go, no grpc/protobuf)
    ├── client.go        # PodCIDRAllocationClient interface + error taxonomy
    ├── fake.go          # LoggingFakeClient (ships this drop) + test knobs
    ├── fake_test.go
    └── mock/
        └── client.go    # mockgen-generated (go:generate, gomock — go.mod golang/mock v1.6.0)
```

No cilium dependency is added. CiliumNode is consumed via the dynamic client (`k8s.io/client-go/dynamic` + `dynamicinformer`, both already vendored: `vendor/k8s.io/client-go/dynamic/simple.go:75`, `vendor/k8s.io/client-go/dynamic/dynamicinformer/informer.go:36`).

## 4. `sdn` Package — Interface & Types (mirrors merged proto, schemas MR 4075)

Shapes below mirror `island.v2.region.PodCIDRAllocationManagement` as merged. When the generated client is adopted (§14), only `sdn/grpc.go` maps between these structs and the pb types; the state machine is unchanged.

```go
// internal/routes/sdn/types.go
package sdn

import "time"

type OperationState string // mirrors island.v2.component.OperationState

const (
	OperationStateInProgress OperationState = "IN_PROGRESS"
	OperationStateSucceeded  OperationState = "SUCCEEDED"
	OperationStateFailed     OperationState = "FAILED"
)

// Operation mirrors island.v2.component.Operation as used by
// PodCIDRAllocationManagement. Only OVN/DB failures land in the operation
// result; validation failures fail the RPC synchronously and write nothing.
type Operation struct {
	OperationID   string
	State         OperationState
	AllocationIDs []string // pod CIDR allocation ids the operation acts on
	Error         string   // set iff State == FAILED
}

// PodCIDRAllocationContext scopes every RPC. A project mismatch → NOT_FOUND.
type PodCIDRAllocationContext struct {
	ProjectID    string
	VPCNetworkID string
	Location     string
}

// PodCIDRAllocationSpec — all fields required. The reservation must contain
// destination_cidr; the NIC must be in the same vpc+location, PRIMARY, with a
// private IP; host bits must be zero; destination is unique per VPC.
type PodCIDRAllocationSpec struct {
	VPCPrefixReservationID string
	NetworkInterfaceID     string
	DestinationCIDR        string
}

// PodCIDRAllocation = static route on the VPC logical router + port_security
// widening on the NIC, created/deleted as an atomic pair. Immutable (no update
// RPC). NextHopIP is output-only.
type PodCIDRAllocation struct {
	ID                     string
	Context                PodCIDRAllocationContext
	VPCPrefixReservationID string
	NetworkInterfaceID     string
	DestinationCIDR        string
	NextHopIP              string
	CreatedAt              time.Time
}

// CreatePodCIDRAllocationsRequest: batch, all-or-nothing, ONE OVN transaction.
// Capped at 1 allocation per call FOR NOW (>1 today = INVALID_ARGUMENT; the
// repeated shape lets the cap rise without contract change).
type CreatePodCIDRAllocationsRequest struct {
	Allocations []PodCIDRAllocationSpec
	Context     PodCIDRAllocationContext
}

// DeletePodCIDRAllocationsRequest: batch by id, at most 50, all-or-nothing,
// one OVN transaction. Intent-based: an id that no longer exists is skipped
// (success). An id in another vpc/location fails the WHOLE call NOT_FOUND.
type DeletePodCIDRAllocationsRequest struct {
	IDs     []string
	Context PodCIDRAllocationContext
}

// ListPodCIDRAllocationsQuery: at least ONE bounding filter is required
// (Location alone does NOT count) else INVALID_ARGUMENT. Results ordered by
// destination_cidr. Empty result = success. Zero-valued fields are unset;
// set fields intersect.
type ListPodCIDRAllocationsQuery struct {
	PodCIDRAllocationIDs    []string
	ProjectID               string
	VPCNetworkID            string
	Location                string
	VPCPrefixReservationIDs []string
	NetworkInterfaceID      string
	DestinationCIDR         string // exact match
}

type ListPodCIDRAllocationOperationsQuery struct {
	OperationIDs         []string
	ProjectIDs           []string
	PodCIDRAllocationID  string
	OperationStates      []OperationState
}
```

```go
// internal/routes/sdn/client.go
package sdn

import (
	"context"
	"errors"
)

// Error taxonomy (from the merged proto header). The real client (§14) maps
// gRPC status codes + google.rpc.ErrorInfo to these sentinels; the fake
// returns them directly. Contract guarantees:
//   - ALREADY_EXISTS is NEVER returned. An identical create is idempotent
//     success (never creates a second allocation). Delete of an absent id is
//     success. "Already done" always succeeds (intent-based).
//   - Validation failures (INVALID_ARGUMENT etc.) fail the RPC synchronously
//     and write nothing — permanent, do not retry until inputs change.
//   - ABORTED / UNAVAILABLE are retryable with backoff.
var (
	// ErrDestinationConflict: destination_cidr is held by a DIFFERENT
	// interface — FAILED_PRECONDITION with google.rpc.ErrorInfo reason
	// DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE. Callers MUST branch on the
	// ErrorInfo REASON, not the code (FAILED_PRECONDITION is also the
	// unclassified bucket). For the CCM this is RETRYABLE-WITH-BACKOFF, not
	// permanent: the stale allocation clears when the old VM's delete lands.
	ErrDestinationConflict = errors.New("destination cidr allocated to another interface")
	// ErrInvalidArgument: request validation failure. Permanent.
	ErrInvalidArgument = errors.New("invalid pod cidr allocation request")
	// ErrNotFound: context project mismatch, or a delete batch containing an
	// id in another vpc/location (fails the whole call).
	ErrNotFound = errors.New("pod cidr allocation not found in context")
	// ErrUnavailable: ABORTED/UNAVAILABLE-class transient failure. Retryable.
	ErrUnavailable = errors.New("pod cidr allocation service unavailable")
)

//go:generate mockgen -source=client.go -destination=mock/client.go

// PodCIDRAllocationClient is the CCM-side seam over the merged SDN service
// island.v2.region.PodCIDRAllocationManagement (schemas MR 4075).
type PodCIDRAllocationClient interface {
	// CreatePodCIDRAllocations is async: the returned Operation starts
	// IN_PROGRESS. Safe to retry: an identical request never creates a second
	// allocation. This controller always sends exactly 1 spec (§1 cap).
	CreatePodCIDRAllocations(ctx context.Context, req CreatePodCIDRAllocationsRequest) (*Operation, error)
	// DeletePodCIDRAllocations is async and intent-based (absent ids are
	// skipped as success). REAPER USE ONLY (§11) — the reconcile path never
	// deletes; VM delete owns per-node cleanup (KM MR 1314).
	DeletePodCIDRAllocations(ctx context.Context, req DeletePodCIDRAllocationsRequest) (*Operation, error)
	ListPodCIDRAllocations(ctx context.Context, q ListPodCIDRAllocationsQuery) ([]PodCIDRAllocation, error)
	// ListPodCIDRAllocationOperations supports repeated operation_ids — ONE
	// call covers every in-flight operation (the opTracker relies on this).
	ListPodCIDRAllocationOperations(ctx context.Context, q ListPodCIDRAllocationOperationsQuery) ([]Operation, error)
}
```

### 4.1 LoggingFakeClient behavior spec (`sdn/fake.go`)

```go
type LoggingFakeClient struct {
	mu     sync.Mutex
	allocs map[string]PodCIDRAllocation // allocation id → allocation
	ops    map[string]*fakeOp           // op id → op + remaining polls

	// PendingPolls: number of ListPodCIDRAllocationOperations observations an
	// operation stays IN_PROGRESS before flipping terminal. Default 1. Test knob.
	PendingPolls int
	// FailNext: if true, the NEXT Create/Delete's operation resolves FAILED
	// (and the mutation is rolled back); flag auto-clears. Test knob.
	FailNext bool
	// ConflictCIDRs: destinations for which CreatePodCIDRAllocations returns
	// ErrDestinationConflict, simulating a stale allocation held by a dead
	// VM's NIC. Test knob for the /24-reuse race.
	ConflictCIDRs map[string]bool
}

func NewLoggingFakeClient() *LoggingFakeClient
var _ PodCIDRAllocationClient = (*LoggingFakeClient)(nil)
```

Semantics (all methods take the mutex; ids are `uuid.NewString()` — `github.com/google/uuid` already in the module graph). The fake honors the contract's **intent-based** semantics exactly, so tests exercise the real branching:

| Method | Behavior |
|---|---|
| `CreatePodCIDRAllocations` | `len(Allocations) != 1` → `ErrInvalidArgument` (current cap). Destination in `ConflictCIDRs`, or an existing allocation with same destination but **different** NIC → `ErrDestinationConflict` (synchronous, nothing written). An **identical** spec (same reservation+NIC+destination) already allocated → idempotent success: op resolving `SUCCEEDED` referencing the existing id, no second row. Else insert allocation (generated id, `NextHopIP` = `"fake-next-hop"`), op `IN_PROGRESS`, remaining = `PendingPolls`. `FailNext` ⇒ op resolves `FAILED` and the row is rolled back at resolution. |
| `DeletePodCIDRAllocations` | >50 ids → `ErrInvalidArgument`. Unknown ids are **skipped (success)** — never an error. One op for the whole batch; rows removed when it resolves `SUCCEEDED`. `FailNext` ⇒ `FAILED`, rows retained. |
| `ListPodCIDRAllocations` | No bounding filter set (Location alone doesn't count) → `ErrInvalidArgument`. Applies every set field as an intersecting filter; results sorted by `DestinationCIDR`. Rows pending a delete op are listed until it resolves. |
| `ListPodCIDRAllocationOperations` | Filters ops (batch `OperationIDs` supported); each matched `IN_PROGRESS` op decrements its remaining counter, flipping terminal at 0 (this is what makes the poll loop observable in tests). |

Every method emits exactly one structured klog line describing the RPC that *would* be sent, before applying table changes:

```
I0828 ... fake.go:87] "SDN RPC (fake)" rpc="CreatePodCIDRAllocations" project_id="proj-123" vpc_network_id="net-abc" location="us-east1-a" vpc_prefix_reservation_id="rsv-pods" network_interface_id="nic-1" destination_cidr="10.100.4.0/24" operation_id="op-7f3a" allocation_id="al-91c2"
I0828 ... fake.go:132] "SDN RPC (fake)" rpc="DeletePodCIDRAllocations" project_id="proj-123" vpc_network_id="net-abc" location="us-east1-a" ids=["al-91c2"] operation_id="op-c001"
I0828 ... fake.go:160] "SDN RPC (fake)" rpc="ListPodCIDRAllocations" vpc_prefix_reservation_ids=["rsv-pods"] destination_cidr="" matches=3
I0828 ... fake.go:190] "SDN RPC (fake)" rpc="ListPodCIDRAllocationOperations" operation_ids=["op-7f3a","op-c001"] matches=2 states=["SUCCEEDED","IN_PROGRESS"]
```

Use `klog.InfoS` (structured), consistent with klog usage across the repo (`internal/client/client.go:13`).

## 5. Configuration (`config.go`)

Env-var pattern follows `internal/cloud.go:14-19` (consts) and `internal/client/client.go:16-18` (`CRUSOE_PROJECT_ID`).

The crusoe-ccm Deployment rendered by addon-controller (MR 57, merged direction) sets **exactly** three routing vars: `CRUSOE_ROUTING_MODE`, `CRUSOE_VPC_PREFIX_RESERVATION_ID`, `CRUSOE_VPC_ID`. All three are always set together in native mode and absent in overlay — **a partial set must fail startup loudly** (misrendered config, not a mode choice).

```go
const (
	RoutingModeEnv            = "CRUSOE_ROUTING_MODE"              // "overlay" (default) | "native"
	VPCIDEnv                  = "CRUSOE_VPC_ID"                    // → PodCIDRAllocationContext.vpc_network_id
	VPCPrefixReservationIDEnv = "CRUSOE_VPC_PREFIX_RESERVATION_ID" // KM-provisioned (VPCPrefixReservationManagement)
	// No location env var: location (and the context's project id) are derived
	// from instance metadata (§5.1).
	// Defined but unused this drop (real gRPC client wiring):
	SDNEndpointEnv       = "CRUSOE_SDN_ENDPOINT"
	SDNClientCertPathEnv = "CRUSOE_SDN_CLIENT_CERT_PATH"
	SDNClientKeyPathEnv  = "CRUSOE_SDN_CLIENT_KEY_PATH"
	SDNCACertPathEnv     = "CRUSOE_SDN_CA_CERT_PATH"

	RoutingModeOverlay = "overlay"
	RoutingModeNative  = "native"
)

type Config struct {
	RoutingMode            string
	ProjectID              string // from CRUSOE_PROJECT_ID (internal/client/client.go:17; already rendered, v0.1.2.yaml:76)
	VPCID                  string // context.vpc_network_id
	VPCPrefixReservationID string
	Location               string // resolved at startup from the cluster object; instance-derived fallback (§5.1)
	SDNEndpoint            string // unused this drop
	SDNClientCertPath, SDNClientKeyPath, SDNCACertPath string // unused this drop

	PollInterval   time.Duration // default 5s (opTracker tick + AddAfter backstop)
	ReaperInterval time.Duration // default 5m
	ReaperGrace    time.Duration // default 10m (§11 step 4)
	Workers        int           // default 4
}

var (
	ErrMissingConfig      = errors.New("missing required environment variable for native routing mode")
	ErrInconsistentConfig = errors.New("partial native-routing env set (CRUSOE_ROUTING_MODE / CRUSOE_VPC_ID / CRUSOE_VPC_PREFIX_RESERVATION_ID must be all set or all absent)")
)

// LoadConfigFromEnv:
//   overlay mode: returns (cfg, nil) — but if CRUSOE_VPC_ID or
//     CRUSOE_VPC_PREFIX_RESERVATION_ID is set anyway → ErrInconsistentConfig.
//   native mode: CRUSOE_VPC_ID, CRUSOE_VPC_PREFIX_RESERVATION_ID and
//     CRUSOE_PROJECT_ID must all be non-empty, else
//     fmt.Errorf("%w: %s", ErrMissingConfig, name). Location is always
//     derived from instance metadata (§5.1), never from env.
func LoadConfigFromEnv() (*Config, error)
```

Fail-fast: `StartRouteControllerWrapper` returns the error, which aborts CCM startup (the app framework treats an `InitFunc` error as fatal).

### 5.1 project_id / location sourcing — derived from platform metadata, no new env

`PodCIDRAllocationContext` needs `project_id` and `location`, which MR 57 does **not** render. Both are derived from what the CCM already has, rather than asking addon-controller for more env:

- **project_id**: `CRUSOE_PROJECT_ID` stays required at startup — the instance client itself needs it (`internal/client/client.go:37-40` errors without it). The context's `project_id` is taken from the resolved instance's `ProjectId` (authoritative; `klog.Warningf` on mismatch with the env value — should never happen).
- **location, primary — the cluster object, at startup, before any node joins.** The CCM runs in the context of a cluster, and the cluster already has a location. The deployment already passes `--cluster-name` (`releases/crusoe-cloud-controller-manager/v0.1.2.yaml:69`), surfaced in code as `completedConfig.ComponentConfig.KubeCloudShared.ClusterName`. At controller startup (§6.2): `KubernetesClustersApi.ListClusters(ctx, cfg.ProjectID)` (`vendor/github.com/crusoecloud/client-go/swagger/v1alpha5/api_kubernetes_clusters.go:518`) → match `KubernetesCluster.Name == ClusterName` → `Config.Location = cluster.Location` (`model_kubernetes_cluster.go:23`; cluster names are unique per project). Requires one new `APIClient` method `GetClusterByName(ctx, projectID, name)` following the `GetInstanceByName` list-and-filter pattern (`internal/client/client.go:35-60`) + mock regen. Failure to resolve at startup is **non-fatal** (logged at Error): reconciles don't need it yet (fallback below), and the next reaper pass retries the lookup.
- **location, fallback/cross-check — instance metadata.** Every allocation RPC for a node is preceded by resolving that node's instance for the NIC id, so `InstanceV1Alpha5.Location` (`model_instance_v1_alpha5.go:22`; `ProjectId` at :27) is available in the same call: if `Config.Location` is still empty, first `resolveNIC` success fills it (under the controller mutex); if non-empty, mismatch logs a warning (cluster lookup vs instance disagreeing would mean a mis-rendered `--cluster-name`).
- **Contract note (record in the addon-controller/clusterlet review):** the rendered `--cluster-name` must equal the Crusoe cluster resource name — that equality is what makes the startup lookup work. The sample manifest already does this (v0.1.2.yaml:69, `sriprod1`).
- The reaper skips its pass with a warning while `Location == ""` — with the startup lookup this only happens if `ListClusters` is failing, and it self-heals on a later pass or the first node join.

Rejected alternative: deriving location from the VPC's subnets (`VpcNetwork.Subnets` → `VpcSubnet.Location`) — a VPC network is not location-bound, so a VPC with subnets in more than one location makes the answer ambiguous; the cluster object is the unambiguous source.

## 6. Controller Struct, Construction & Registration

### 6.1 Struct (`controller.go`)

```go
const (
	// Applied by kubelet via --register-with-taints; the CCM only removes it.
	PodsUnroutableTaintKey = "crusoe.ai/pods-unroutable"

	// OpIDLabel carries the in-flight CreatePodCIDRAllocations operation id,
	// written immediately after the create RPC returns. Its presence is the
	// invariant "an allocation create is in flight for this node".
	OpIDLabel = "crusoe.ai/pod-cidr-allocation-op-id"
	// ReadyAtLabel is the CCM-observed completion time as Unix epoch SECONDS
	// (decimal string — RFC3339 is an illegal label value, colons fail the
	// label-value regex), set in the same final patch that removes the taint.
	ReadyAtLabel = "crusoe.ai/pod-cidr-allocation-ready-at"

	controllerName = "crusoe-route-controller"
)
```

**Labels, with epoch time** (recorded decision): both values live in Node **labels** so they are label-selectable (`kubectl get nodes -l 'crusoe.ai/pod-cidr-allocation-op-id'` lists nodes with a create in flight). The timestamp is stored as Unix epoch seconds because an RFC3339 timestamp is an *illegal* label value (colons fail the label-value regex `(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?`); an operation-id UUID and a decimal epoch are both legal (≤63 chars, allowed charset). Trade-off, accepted: node-label writes fan out to every node watcher and labels are scheduling surface — the write rate here is bounded by node churn (2 label writes per node join), which is negligible. Labels carry the crash-recovery benefit: after leader failover the new leader reads the op id off the Node and resumes polling instead of blind re-create.

```go
//nolint:gochecknoglobals // can't construct const structs (same as internal/node/node_lifecycle_controller.go:33-37)
var PodsUnroutableTaint = &v1.Taint{
	Key:    PodsUnroutableTaintKey,
	Effect: v1.TaintEffectNoSchedule,
}

//nolint:gochecknoglobals
var ciliumNodeGVR = schema.GroupVersionResource{
	Group: "cilium.io", Version: "v2", Resource: "ciliumnodes",
}

type RouteController struct {
	cfg        *Config
	kubeClient clientset.Interface
	dynClient  dynamic.Interface
	sdn        sdn.PodCIDRAllocationClient
	apiClient  client.APIClient // NIC resolution (internal/client/client.go:29-33)

	ciliumNodeLister  cache.GenericLister // dynamic informer lister (unstructured)
	ciliumNodesSynced cache.InformerSynced
	nodeLister        v1lister.NodeLister
	nodesSynced       cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string] // key: node name

	tracker *opTracker // §7.1 — single batched poll loop, no per-node goroutines

	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder

	mu    sync.Mutex            // guards state + cfg.Location derivation
	state map[string]*nodeState // key: node name; soft cache only
}

// nodeState is soft state; everything durable lives on the Node (labels)
// or in SDN (List APIs). Rebuilt on restart.
type nodeState struct {
	nicID           string    // immutable per instance once resolved
	allocationID    string    // "" until known (from op result or List)
	createStart     time.Time // for crusoe_pod_cidr_allocation_provision_seconds
	ready           bool      // condition set + final patch applied
	warnedMultiCIDR bool
	conflictSince   time.Time // first ErrDestinationConflict; zero if none (event dedup)
}
```

```go
func NewRouteController(
	cfg *Config,
	kubeClient clientset.Interface,
	dynClient dynamic.Interface,
	sdnClient sdn.PodCIDRAllocationClient,
	apiClient client.APIClient,
	ciliumNodeInformer informers.GenericInformer, // dynamicinformer factory.ForResource(ciliumNodeGVR)
	nodeInformer coreinformers.NodeInformer,
) (*RouteController, error)

// Run blocks; call via goroutine. Starts broadcaster, waits for cache sync,
// launches cfg.Workers reconcile workers + the opTracker poll loop + 1 reaper
// goroutine, all stopping on ctx cancellation.
func (c *RouteController) Run(ctx context.Context,
	controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics)
```

Event recorder construction follows `internal/node/node_lifecycle_controller.go:69-70` (`record.NewBroadcaster()` + `NewRecorder(scheme.Scheme, v1.EventSource{Component: controllerName})`); pipeline start/stop follows `node_lifecycle_controller.go:108-111` (`StartStructuredLogging(0)`, `StartRecordingToSink(&v1core.EventSinkImpl{...})`, `defer broadcaster.Shutdown()`). `ControllerStarted/Stopped` metrics follow `node_lifecycle_controller.go:104-105`.

### 6.2 Registration wrapper (`register.go`)

Mirrors `internal/node/utils.go:20-30` exactly (`app.InitFunc` is `func(ctx, controllermanagerapp.ControllerContext) (controller.Interface, bool, error)` — `vendor/k8s.io/cloud-provider/app/controllermanager.go:368`):

```go
func StartRouteControllerWrapper(initContext app.ControllerInitContext,
	completedConfig *config.CompletedConfig,
	cloud cloudprovider.Interface,
) app.InitFunc {
	return func(ctx context.Context,
		controllerContext controllermanagerapp.ControllerContext,
	) (controller.Interface, bool, error) {
		return startRouteController(ctx, initContext, controllerContext, completedConfig, cloud)
	}
}
```

`startRouteController` logic:

1. `cfg, err := LoadConfigFromEnv()`; on error `return nil, false, err` (fatal — fail fast; catches the partial-env case from §5).
2. If `cfg.RoutingMode != RoutingModeNative`: `klog.Infof("crusoe-route-controller disabled: CRUSOE_ROUTING_MODE=%q (native routing not enabled)", cfg.RoutingMode)`; `return nil, false, nil` (not enabled — same convention as `internal/node/utils.go:56-58`).
3. Build clients from `completedConfig.Kubeconfig`, same precedent as the node-lifecycle controller which sidesteps per-controller SA credentials (`internal/node/utils.go:39-42`): `clientset.NewForConfig(...)` and `dynamic.NewForConfig(...)` (`vendor/k8s.io/client-go/dynamic/simple.go:75`).
4. Build the Crusoe API client exactly as `internal/cloud.go:63-71` does (`auth.NewCrusoeClient` + `&client.APIClientImpl{...}`) — the `cloud` parameter does not expose its client, and duplicating 4 lines is cheaper than widening `Cloud`'s surface.
5. **Resolve location from the cluster object** (§5.1): `apiClient.GetClusterByName(ctx, cfg.ProjectID, completedConfig.ComponentConfig.KubeCloudShared.ClusterName)` → `cfg.Location = cluster.Location`. Non-fatal on error (log at Error; instance-derived fallback covers reconciles, the reaper retries the lookup each pass while `Location == ""`).
6. `sdnClient := sdn.NewLoggingFakeClient()` — the single line replaced when the real client lands (§14).
7. Dynamic informer factory: `dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynClient, 30*time.Minute, metav1.NamespaceAll, nil)` (`vendor/k8s.io/client-go/dynamic/dynamicinformer/informer.go:42`); `ciliumNodeInformer := factory.ForResource(ciliumNodeGVR)` (informer.go:74). Node informer from `completedConfig.SharedInformers.Core().V1().Nodes()` (pattern: `internal/node/utils.go:47`).
8. `NewRouteController(...)`; `factory.Start(ctx.Done())`; `go rc.Run(ctx, controllerContext.ControllerManagerMetrics)`; `return nil, true, nil`.

### 6.3 Exact `main.go` change

After the existing node-lifecycle override (`cmd/crusoe-cloud-controller-manager/main.go:35-41`), before `app.NewCloudControllerManagerCommand` (main.go:43):

```go
// Register the Crusoe VPC route controller (VPC-native pod routing, CRUSOE-97212).
// No-ops unless CRUSOE_ROUTING_MODE=native.
app.DefaultInitFuncConstructors["crusoe-route-controller"] = app.ControllerInitFuncConstructor{
	InitContext: app.ControllerInitContext{
		ClientName: "crusoe-route-controller",
	},
	Constructor: routes.StartRouteControllerWrapper,
}
```

plus import `"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes"`. Controllers in `DefaultInitFuncConstructors` (`vendor/k8s.io/cloud-provider/app/controllermanager.go:441-442`) are enabled by default (`--controllers=*`). The upstream `node-route-controller` entry remains; it self-disables because `--configure-cloud-routes` defaults are not set and `Routes()` returns `nil, false` (`internal/cloud.go:43-45`). `ClientName` is cosmetic here because clients are built from `completedConfig.Kubeconfig` (step 3 above), matching the node-lifecycle precedent.

## 7. Informer / Workqueue / opTracker Topology

```
CiliumNode dynamic informer (cilium.io/v2 ciliumnodes, cluster-scoped, resync 30m)
        │ Add/Update → enqueue(name); Delete → clear soft state only (no SDN calls)
        ▼
workqueue.TypedRateLimitingInterface[string] ── key = node name ──► N workers → reconcile(key)
        ▲    ▲                                                            │
        │    │ queue.Add(nodeKey) on terminal op                          │ AddAfter(key, PollInterval)
        │    │                                                            │ backstop while op in flight
        │  opTracker (ONE goroutine): every PollInterval, if pending ≠ ∅:
        │  ListPodCIDRAllocationOperations(operation_ids=[ALL pending])   │
        │    — one batched RPC for every in-flight create                 │
        │                                                                 │
        │ Add(name) on relevant Node events                               │
Node informer (completedConfig.SharedInformers.Core().V1().Nodes())
        ▲
reaper (every ReaperInterval): batch-deletes orphaned allocations (≤50/chunk,
grace-period-gated), enqueues keys for missing/mismatched, sets gauges
```

- **Key**: node name. CiliumNode name == Node name (cilium invariant; also how NIC lookup by name works, `internal/client/client.go:41` strips domain suffix).
- **CiliumNode handlers**: enqueue on Add and Update (always — level-triggered, reconcile is cheap when nothing changed). Delete clears `state[name]` and any tracker registration (tombstone-aware via `cache.DeletedFinalStateUnknown`) — **nothing else**: allocation deletion is owned by the VM delete flow (KM MR 1314).
- **Node handlers**: enqueue on Add and on Update only when the node still carries `PodsUnroutableTaint` or lacks `NetworkUnavailable=False` — this re-drives steps that need the Node object if the Node registers after the allocation is ready. Also re-registers label op-ids with the tracker on informer sync (crash/failover recovery, §10). No Delete handler.
- **Resync 30m** on both informers gives a level-triggered backstop independent of the reaper.
- **Workqueue**: `workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())` (`vendor/k8s.io/client-go/util/workqueue/rate_limiting_queue.go:77`, `default_rate_limiters.go:50` — 5ms→1000s per-item exponential + 10 qps/100 burst overall). The workqueue guarantees a key is never processed by two workers concurrently, so per-node state needs only the `state` map mutex.
- **Polling**: reconcile never blocks on an operation. With an op in flight it returns a `requeueAfter` and the worker calls `queue.AddAfter(key, cfg.PollInterval)` (`vendor/k8s.io/client-go/util/workqueue/delaying_queue.go:39`) — deliberate re-check, not an error, so it bypasses the rate limiter and `Forget` is called. The tracker's `queue.Add` on terminal ops makes completion prompt; `AddAfter` is the backstop if the tracker misses (restart between patch and Track).

Worker loop contract:

```go
// reconcile returns (requeueAfter, err):
//   err != nil            → queue.AddRateLimited(key)   (backoff)
//   requeueAfter > 0      → queue.Forget(key); queue.AddAfter(key, requeueAfter)
//   both zero             → queue.Forget(key)           (done)
func (c *RouteController) reconcile(ctx context.Context, nodeName string) (time.Duration, error)
```

### 7.1 opTracker (`optracker.go`)

**Rejected alternative — one goroutine per node/operation**: at a 500-node pool join that is 500 concurrent pollers stampeding the region API with identical single-op queries, and goroutines are edge-triggered state that dies with the process (leader failover loses every poller). Rejected.

Instead, a single poll loop exploiting the batch shape of `ListPodCIDRAllocationOperations` (repeated `operation_ids`): **one RPC per tick covers every in-flight create**, regardless of node count.

```go
type opTracker struct {
	mu      sync.Mutex
	pending map[string]trackedOp  // opID → {nodeKey, misses}
	results map[string]sdn.Operation // terminal ops awaiting consumption by reconcile
}

// Track registers an in-flight op for a node. Idempotent.
func (t *opTracker) Track(opID, nodeKey string)
// TakeResult returns and removes a terminal result if the tracker has seen one.
func (t *opTracker) TakeResult(opID string) (sdn.Operation, bool)
// Forget drops any registration/result for the op (node deleted, op consumed).
func (t *opTracker) Forget(opID string)
```

Poll loop, run from `Run` via `wait.UntilWithContext(ctx, c.pollOpsOnce, cfg.PollInterval)` (same pattern as `internal/node/node_lifecycle_controller.go:119`):

1. Snapshot pending op ids under the mutex; if empty, return (no RPC).
2. `ListPodCIDRAllocationOperations(OperationIDs: allPending)` — one call.
3. For each returned terminal (`SUCCEEDED`/`FAILED`) op: move pending → results, `queue.Add(nodeKey)` (prompt dispatch; reconcile consumes via `TakeResult`).
4. Op ids **not returned** (retention gap / SDN restart): increment `misses`; after 3 consecutive misses drop from pending and `queue.Add(nodeKey)` — reconcile's List-based recovery (§10 C6) takes over.
5. RPC error: log at Error, leave pending intact (next tick retries; per-node `AddAfter` backstop still drives reconciles).

Registration points: reconcile after a successful create + label patch (C8), and recovery re-registration when reconcile encounters an op-id label the tracker doesn't know (C6).

## 8. CiliumNode Access (`ciliumnode.go`)

Read-only. With the finalizer removed (KM MR 1314 owns deletion), the CCM never writes CiliumNode — conversion is all that remains. Unit-tested against `testdata/ciliumnode.json` (captured from a real cluster):

```go
type ciliumNode struct {
	Name              string
	UID               types.UID
	ResourceVersion   string
	DeletionTimestamp *metav1.Time
	PodCIDRs          []string // spec.ipam.podCIDRs
}

var ErrNotCiliumNode = errors.New("object is not a CiliumNode unstructured")

// ciliumNodeFromUnstructured extracts the fields above using
// unstructured.Nested* accessors. Missing spec.ipam.podCIDRs is NOT an error
// (returns empty slice) — cilium fills it asynchronously.
func ciliumNodeFromUnstructured(u *unstructured.Unstructured) (*ciliumNode, error)
```

## 9. NIC Resolution (`nic.go`)

```go
// resolveNIC returns the network_interface_id for nodeName, caching in
// nodeState.nicID (immutable per instance). Resolution order:
//  1. Node.Spec.ProviderID ("crusoe://<uuid>", internal/instances/instances.go:21)
//     → strip prefix → apiClient.GetInstanceByID (internal/client/client.go:77)
//  2. Node.Status.NodeInfo.SystemUUID (same fallback rationale as
//     internal/instances/instances.go:236-258) → GetInstanceByID
//  3. apiClient.GetInstanceByName(nodeName) (internal/client/client.go:35)
// Then pick the NIC whose Network == cfg.VPCID
// (crusoeapi.NetworkInterface{Id, Network, ...},
//  vendor/github.com/crusoecloud/client-go/swagger/v1alpha5/model_network_interface.go:11-19);
// if none matches, fall back to NetworkInterfaces[0] with klog.Warningf.
// Node may be nil (not yet registered) → skip steps 1-2.
//
// Side effect (§5.1): fallback/cross-check for the startup cluster lookup —
// if cfg.Location is still empty, first success sets it from
// instance.Location (model_instance_v1_alpha5.go:22) under c.mu; if
// non-empty, a mismatch logs a warning (mis-rendered --cluster-name). The
// context's project id comes from instance.ProjectId (:27), authoritative,
// with a warning if it differs from CRUSOE_PROJECT_ID.
func (c *RouteController) resolveNIC(ctx context.Context, nodeName string, node *v1.Node) (string, error)

var ErrNoNetworkInterfaces = errors.New("instance has no network interfaces")
```

Errors are wrapped (`fmt.Errorf("failed to resolve NIC for node %s: %w", ...)`) and surface as reconcile errors → rate-limited retry.

## 10. Reconcile State Machine (`reconcile.go`)

Create-only. `reconcile(ctx, nodeName)` — numbered; every step is idempotent and safe to re-enter. Invariant: **op-id label present ⇔ a create is (believed) in flight**; the final patch (C9) clears it.

**Fetch & dispatch**

1. Get CiliumNode from `ciliumNodeLister` (`.Get(nodeName)`, cluster-scoped). **Not found or `DeletionTimestamp != nil`** → delete `state[nodeName]`, `tracker.Forget` any op registered for it, return `(0, nil)`. No SDN calls: the VM delete removes the allocations SDN-side (KM MR 1314); anything leaked is the reaper's job (§11).
2. Convert via `ciliumNodeFromUnstructured`. Conversion error → return err (rate-limited; likely a CRD schema surprise, logged at Error).

**Create path**

- **C1.** `podCIDRs` empty → return `(0, nil)`. Purely event-driven: cilium's update to `spec.ipam.podCIDRs` triggers an Update event. Log at `klog.V(4)`.
- **C2.** `len(podCIDRs) > 1` → route `podCIDRs[0]` only; `klog.Warningf` once per node (`warnedMultiCIDR`).
- **C3.** Get Node from `nodeLister.Get(nodeName)`. `NotFound` → `node = nil` and continue (allocation creation must not wait for kubelet registration; C9 is re-driven by the Node informer Add handler). If `node != nil` and it already has `ReadyAtLabel` + no taint + `NetworkUnavailable=False` → mark `state.ready`, return `(0, nil)` (steady-state fast path, no SDN calls).
- **C4.** `resolveNIC(ctx, nodeName, node)`. Error → return err (rate-limited retry).
- **C5.** Read `OpIDLabel` off the Node (`node == nil` ⇒ treat as absent — the label can only have been written if the Node existed).
- **C6.** **Label op-id present** — resume the in-flight op:
  - `op, ok := tracker.TakeResult(opID)`; if `!ok` and the tracker isn't polling it (leader failover / restart) → `tracker.Track(opID, nodeName)`, return `(PollInterval, nil)`.
  - `IN_PROGRESS` (tracker still pending) → return `(PollInterval, nil)`.
  - `SUCCEEDED` → `state.allocationID = op.AllocationIDs[0]`, observe `crusoe_pod_cidr_allocation_provision_seconds` (if `createStart` nonzero), go to C9.
  - `FAILED` (OVN/DB failure — the only kind that lands in an operation result) → patch Node to **clear op-id label**, emit `AllocationCreateFailed` Warning event with `op.Error`, `..._reconcile_total{result="error"}`, return err → rate-limited retry re-creates (create is contractually idempotent, safe to re-issue).
  - **Op expired/unknown SDN-side** (tracker dropped it after misses, §7.1 step 4): fall back to `ListPodCIDRAllocations(DestinationCIDR: podCIDRs[0], VPCPrefixReservationIDs: [cfg.VPCPrefixReservationID])`:
    - allocation exists with **our** NIC → adopt id, go to C9 (create succeeded, op history lost).
    - exists with a **different** NIC → C10 (conflict).
    - absent → clear op-id label, go to C8 (re-create).
- **C7.** **No label** and not ready — the create may have succeeded before the label write (crash window), so **List before create**: `ListPodCIDRAllocations(DestinationCIDR: podCIDRs[0], VPCPrefixReservationIDs: [cfg.VPCPrefixReservationID])` (both filters bounding, satisfies the ≥1-bounding-filter rule):
  - found, NIC == ours → adopt id, go to C9.
  - found, NIC ≠ ours → C10 (the /24-reuse race, or a leak — same handling).
  - absent → C8.
- **C8.** **Create**: `CreatePodCIDRAllocations{Allocations: [{cfg.VPCPrefixReservationID, nicID, podCIDRs[0]}], Context: {cfg.ProjectID, cfg.VPCID, cfg.Location}}` (exactly 1 spec — current cap):
  - `ErrDestinationConflict` → C10.
  - `ErrInvalidArgument` / other validation → permanent: `AllocationCreateFailed` Warning event, `klog.ErrorS`, return err. (Still rate-limited requeue — the 1000s cap makes this a slow retry, not a hot loop; a config/reservation fix is needed for it to ever succeed.)
  - Retryable (`ErrUnavailable`) or transport error → return err.
  - Success → `state.createStart = time.Now()` (only if zero); **immediately** patch Node label `OpIDLabel = op.OperationID` (durable before anything depends on it) — if `node == nil`, skip the patch (recovery is C7's List-before-create; identical re-create is contractually a no-op); then `tracker.Track(opID, nodeName)`; return `(PollInterval, nil)`. If the label patch fails after the create RPC succeeded, return err — the retry path is safe by idempotency (C7 List finds it, or re-create matches identically).
- **C9.** **Finalize — condition before taint.** If `node == nil` → return `(0, nil)` (Node informer Add re-enqueues). Else:
  1. Set `NetworkUnavailable=False` via `nodeutil.SetNodeCondition(kubeClient, types.NodeName(nodeName), v1.NodeCondition{Type: v1.NodeNetworkUnavailable, Status: v1.ConditionFalse, Reason: "CrusoePodCIDRAllocated", Message: "crusoe-route-controller programmed SDN pod CIDR allocation"})` (`vendor/k8s.io/component-helpers/node/util/conditions.go:45`; same package already used at `internal/node/node_lifecycle_controller.go:25,138`). Skip the PATCH if the condition already holds (`nodeutil.GetNodeCondition`, conditions.go:32). Error → return err. (Status subresource — cannot be combined with the metadata/spec patch below.)
  2. **One final Node patch, taint removal LAST** (`nodepatch.go`): strategic-merge patch computed old→new (same technique as `PatchNodeTaints`, `vendor/k8s.io/cloud-provider/node/helpers/taints.go`) that simultaneously (a) removes `PodsUnroutableTaint` from `spec.taints`, (b) deletes `OpIDLabel`, (c) sets `ReadyAtLabel = strconv.FormatInt(time.Now().Unix(), 10)` (epoch seconds — legal label value). One write instead of three, and atomicity: `ready-at` is recorded iff the taint actually came off. No-op if already untainted with ready-at set. Error → return err (condition already set; retry redoes only this patch).
- **C10.** **Conflict — destination allocated to another interface.** Expected /24-reuse race: node B received a /24 whose allocation still points at dead node A's NIC (k8s node delete precedes VM delete completion). Handling: set `conflictSince` if zero; emit `AllocationConflict` Warning event (once per episode — guarded by `conflictSince`); `crusoe_pod_cidr_allocation_conflict_total`++ (every occurrence); return err → rate-limited backoff, **node stays tainted**. Self-resolves when the old VM's delete lands SDN-side; the reaper's NIC-mismatch delete (§11) is the backstop if it never does. **Alert on sustained conflict rate** — it indicates a leaked allocation (§13).
- **C11.** First transition to ready (`!state.ready` at C9 completion): `state.ready = true`; emit Normal event `AllocationCreated`; `..._reconcile_total{result="success"}`. Return `(0, nil)`.

**Crash/failover recovery ordering** (restating C5-C7 as the invariant): tainted Node + CiliumNode with podCIDRs →
- op-id label **present** → poll it (batched); if expired/unknown SDN-side → List by destination_cidr: right NIC → untaint path; absent → clear label, re-create.
- op-id label **absent** → List by destination_cidr **first** (create may have succeeded before the label write), create only if absent.

Edge-case table:

| Edge | Handling |
|---|---|
| CiliumNode deleted (or deleting) | Fetch step 1: clear soft state + tracker only; **zero SDN calls** (VM delete owns cleanup) |
| `podCIDRs` empty | C1: wait for CiliumNode update event |
| Multiple podCIDRs | C2: route `[0]`, warn once |
| Node object missing | C3/C8/C9: create allocation anyway; label+condition+taint deferred to Node Add event |
| NIC lookup failure | C4: wrapped error, rate-limited retry |
| No NIC matches `CRUSOE_VPC_ID` | `resolveNIC`: NIC[0] + warning |
| Create op FAILED (OVN/DB) | C6: clear label, event, error → backoff → idempotent re-create |
| Validation failure on create | C8: permanent — event + slow rate-limited retry; needs operator fix |
| `DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE` | C10: event + conflict metric + backoff; node stays tainted; self-resolves on VM delete; reaper backstop |
| Op id unknown to SDN (retention/restart) | C6 fallback: List by destination_cidr |
| Crash between create RPC and label patch | C7: List-before-create finds it; or identical re-create = contractual no-op |
| Leader failover mid-poll | New leader reads op-id label → re-Track → resume polling (no re-create) |
| Crash between condition and final patch | C9 re-entry: condition check no-ops, final patch retried |
| Duplicate create (any reason) | Contractually idempotent success — never a second allocation, never ALREADY_EXISTS |

## 11. Reaper (`reaper.go`)

The reaper is the **only** defense against leaked allocations (VM alive after `kubectl delete node`; VM-delete flows that failed after NIC teardown). It is also the sole caller of `DeletePodCIDRAllocations`.

```go
// runReaper blocks; started from Run as its own goroutine via
// wait.UntilWithContext(ctx, c.reapOnce, c.cfg.ReaperInterval)
// (same pattern as internal/node/node_lifecycle_controller.go:119).
func (c *RouteController) reapOnce(ctx context.Context)
```

Per pass:

1. If `cfg.Location == ""` (not yet derived, §5.1) → skip pass with a warning.
2. **Desired**: list CiliumNodes from lister; for each live one (no `deletionTimestamp`) with non-empty `podCIDRs`, compute `desired[podCIDRs[0]] = nicID` (from `state` cache; on cache miss resolve via `resolveNIC`, on failure skip that node with a warning — never guess).
3. **Actual**: `ListPodCIDRAllocations(VPCPrefixReservationIDs: [cfg.VPCPrefixReservationID])` (bounding filter satisfied). Error → log, abort pass (next tick retries).
4. Set gauges: `crusoe_pod_cidr_allocations_desired = len(desired)`, `crusoe_pod_cidr_allocations_actual = len(actual)`.
5. **Orphans & NIC mismatches**: an allocation is a delete candidate if its `DestinationCIDR ∉ desired` (orphan) **or** `desired[cidr] != alloc.NetworkInterfaceID` (node replaced, /24 reused — delete-then-recreate). **Grace period**: skip candidates with `CreatedAt` within `cfg.ReaperGrace` (default 10m) — protects against racing joins (informer lag between SDN state and CiliumNode listing) and VM deletes already in flight that will remove the allocation themselves. Delete survivors via `DeletePodCIDRAllocations` in chunks of ≤50 ids (all-or-nothing per chunk; intent-based, so ids deleted concurrently by a VM delete are skipped as success). Do not poll the op; the next pass verifies absence. Chunk-level `ErrNotFound` (an id in another vpc/location — should be impossible since we listed within the reservation) → log at Error, abort remaining chunks, next pass re-lists. For each deletion emit `OrphanedAllocationDeleted`: Normal event on the owning Node when one exists (NIC-mismatch case), else structured klog only.
6. **Missing** (desired cidr ∉ actual) and post-delete NIC-mismatch nodes: `queue.Add(nodeName)` — creation always flows through the single reconcile path (C7/C8), so the reaper never races the state machine on creates.

The reaper plus 30m informer resyncs make the system level-triggered end to end.

## 12. Errors, Backoff, Ordering Invariants

- **Error style**: wrapped sentinel / `fmt.Errorf("...: %w", err)` per repo convention (`internal/client/client.go:20-23`, `internal/instances/instances.go`). Package sentinels: `ErrMissingConfig`, `ErrInconsistentConfig`, `ErrNotCiliumNode`, `ErrNoNetworkInterfaces`, plus `sdn.ErrDestinationConflict`, `sdn.ErrInvalidArgument`, `sdn.ErrNotFound`, `sdn.ErrUnavailable`.
- **Retry policy by class** (mirrors the proto's taxonomy): `ErrUnavailable`/transport → normal backoff; `ErrDestinationConflict` → backoff (self-resolving, §10 C10); `ErrInvalidArgument` → backoff degenerates to slow retry (permanent until config fix; surfaced via event); FAILED operations → backoff + idempotent re-create.
- **Backoff**: reconcile errors → `AddRateLimited` (per-item 5ms→1000s exponential, `default_rate_limiters.go:50-53`). Operation waits → `AddAfter(key, PollInterval)` with `Forget` (no penalty for expected waits); tracker `queue.Add` on completion makes waits prompt.
- **Context**: `ctx` from the app framework threads through every SDN/k8s call; workers, tracker loop and reaper exit on `ctx.Done()`.
- **Ordering invariants** (enforced by step order, restated for the implementer):
  1. Op-id label is patched **immediately after** `CreatePodCIDRAllocations` returns, **before** any dependence on the op (C8) — durable across leader failover.
  2. `NetworkUnavailable=False` is set **before** the final patch; taint removal is in the **last** write (C9.1 < C9.2).
  3. `ReadyAtLabel` and taint removal share one patch — ready-at exists iff the taint came off; op-id is cleared in the same patch, preserving the "op-id ⇔ in-flight" invariant.
  4. The reconcile path **never deletes** allocations; only the reaper does, and only past the grace period.
- **Events emitted only on transitions**, never on steady-state re-reconciles (guarded by `state.ready` / `conflictSince`).

## 13. Observability

### Metrics (`metrics.go`)

`k8s.io/component-base/metrics` + `legacyregistry.MustRegister` in `init()` — served on the CCM's existing secure metrics endpoint (prometheus wiring already imported at `cmd/crusoe-cloud-controller-manager/main.go:17-18`). `//nolint:gochecknoglobals` as needed (`.golangci.yml`).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `crusoe_pod_cidr_allocation_reconcile_total` | Counter | `result` ∈ `success\|error\|requeue` | reconcile outcomes |
| `crusoe_pod_cidr_allocation_provision_seconds` | Histogram | — | create issued → op SUCCEEDED; buckets `[1, 2.5, 5, 10, 30, 60, 120, 300, 600]` |
| `crusoe_pod_cidr_allocations_desired` | Gauge | — | set each reaper pass |
| `crusoe_pod_cidr_allocations_actual` | Gauge | — | set each reaper pass |
| `crusoe_pod_cidr_allocation_conflict_total` | Counter | — | `DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE` occurrences (C10) |

**Alerting note**: a brief `conflict_total` burst during node replacement is expected (the /24-reuse race). A **sustained** rate — conflicts continuing past a few reaper intervals — indicates a leaked allocation (VM delete never landed); page on that, the reaper + `OrphanedAllocationDeleted` events are the paper trail.

### Events (on the Node object; skipped when Node absent)

| Reason | Type | When |
|---|---|---|
| `AllocationCreated` | Normal | first transition to ready (C11) |
| `AllocationCreateFailed` | Warning | create op FAILED or synchronous validation failure (C6/C8), includes error detail |
| `AllocationConflict` | Warning | first `DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE` of an episode (C10) |
| `OrphanedAllocationDeleted` | Normal | reaper deleted a stale allocation whose cidr maps to an existing Node (§11 step 5); klog-only otherwise |

### Logging

klog structured (`InfoS`/`ErrorS`) with keys `node`, `cidr`, `allocationID`, `operationID`, `nicID`. Fake logs every would-be RPC at Info (§4.1). State-machine step traces at `V(4)`.

## 14. RBAC & Deployment

The current release manifest binds the CCM ServiceAccount to `cluster-admin` (`releases/crusoe-cloud-controller-manager/v0.1.2.yaml:9-19`), and this controller builds its clients from the CCM kubeconfig (§6.2 step 3), so no RBAC change is strictly required to run. For the least-privilege manifest (next release yaml), the controller needs:

```yaml
- apiGroups: ["cilium.io"]
  resources: ["ciliumnodes"]
  verbs: ["get", "list", "watch"]          # read-only: no finalizer writes anymore
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch", "patch"] # taint removal + op-id/ready-at labels
- apiGroups: [""]
  resources: ["nodes/status"]
  verbs: ["patch"]                         # SetNodeCondition uses PatchStatus
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create", "patch"]
```

Deployment env (native mode; rendered by addon-controller MR 57 alongside the existing `CRUSOE_*` block at v0.1.2.yaml:74-95): `CRUSOE_ROUTING_MODE`, `CRUSOE_VPC_PREFIX_RESERVATION_ID`, `CRUSOE_VPC_ID` — exactly these three, all-or-none (§5). `CRUSOE_PROJECT_ID` is already rendered (v0.1.2.yaml:76). Location and the context's project id are derived from instance metadata (§5.1) — no further env needed. Reserved for the real client: `CRUSOE_SDN_ENDPOINT`, `CRUSOE_SDN_CLIENT_CERT_PATH`, `CRUSOE_SDN_CLIENT_KEY_PATH`, `CRUSOE_SDN_CA_CERT_PATH`.

## 15. Testing Strategy

Runner: `make test` (Makefile, `-race -cover`); lint: `make lint`. Existing test precedent: `internal/instances/instances_test.go` (gomock + testify).

| Layer | Tooling | Coverage |
|---|---|---|
| `sdn` fake | plain Go tests | intent-based contract: identical duplicate create = success without a second row; conflicting-NIC create → `ErrDestinationConflict`; `ConflictCIDRs` knob; delete of unknown ids = success; >1 create spec / >50 delete ids / unbounded List → `ErrInvalidArgument`; batch op poll (`PendingPolls`); `FailNext` → FAILED + rollback; List filter intersection + destination ordering |
| `ciliumnode.go` | fixture test | `testdata/ciliumnode.json` (captured object) → conversion asserts name/deletionTimestamp/`spec.ipam.podCIDRs`; malformed / missing-ipam variants |
| `config.go` | table-driven | overlay default; native with all vars; native missing each var → `ErrMissingConfig`; overlay + stray `CRUSOE_VPC_ID` → `ErrInconsistentConfig` |
| `nic.go` | `mock_client.MockAPIClient` (pattern `internal/client/mock/client.go`) | providerID path, SystemUUID path, name fallback, network match, NIC[0] fallback, zero-NIC error; startup location from `GetClusterByName` (match by name → `.Location`; lookup failure non-fatal); instance-location fallback when empty + mismatch warning when set |
| `opTracker` | fake + fake workqueue | N pending ops → exactly ONE `ListPodCIDRAllocationOperations` call per tick (assert via fake call log); terminal op → result stashed + nodeKey enqueued; unknown op dropped after 3 misses + enqueued; empty pending → zero RPCs |
| reconcile state machine | `k8sfake.NewSimpleClientset` + `dynamicfake` (`vendor/k8s.io/client-go/dynamic/fake/simple.go`) + `LoggingFakeClient` + gomock APIClient; drive `reconcile()` directly (no informers — inject listers built from `cache.NewIndexer`) | happy path C1→C11 incl. write ordering (clientset action list: op-id label patch before any poll; condition PATCH before the final patch; final patch simultaneously removes taint, clears op-id, sets epoch ready-at — value asserted to parse as integer seconds and to pass label-value validation); poll requeue while `PendingPolls>0`; `FailNext` → label cleared + event + error → idempotent recreate; conflict path (knob) → event once + counter + backoff + taint retained; crash recovery: (i) op-id label present + op SUCCEEDED, (ii) op-id present + op expired → List adopt, (iii) no label + allocation pre-seeded in fake → adopt without create; Node absent → allocation created, finalize deferred; CiliumNode deleted → assert **zero** SDN calls; multiple podCIDRs |
| reaper | same harness | orphan past grace deleted (batched, chunk ≤50 asserted with >50 fixtures); young allocation (within grace) spared; NIC-mismatch deleted + node enqueued; missing enqueued; gauges set; List error aborts pass; location-undervied pass skipped |
| envtest-style (optional, follow-up) | real informers against fake clients, `Run()` end-to-end | event handler → queue → worker → tracker wiring, cache-sync gating |

Fake knobs used deliberately: `PendingPolls` proves the controller re-enqueues instead of blocking; `FailNext` proves FAILED→recreate; `ConflictCIDRs` proves the /24-reuse race handling.

## 16. Commit-by-Commit Implementation Plan

Each commit builds, passes `make lint` and `make test` independently.

1. **`internal/routes/sdn`: types, PodCIDRAllocationClient interface, logging fake, gomock** — §4 shapes (merged proto), intent-based fake + `fake_test.go`, `go:generate` + generated `mock/client.go`. No wiring.
2. **`internal/routes`: config loading** — `config.go` + table-driven tests (§5, incl. all-or-none validation).
3. **`internal/routes`: CiliumNode conversion** — `ciliumnode.go` (read-only), `testdata/ciliumnode.json`, tests.
4. **`internal/routes`: NIC resolution + location sourcing** — `nic.go` + tests; add `GetClusterByName(ctx, projectID, name)` to `internal/client.APIClient` (list-and-filter over `KubernetesClustersApi.ListClusters`, pattern `client.go:35-60`); regenerate `internal/client/mock/client.go` (also if `GetInstanceByID` is missing from it).
5. **`internal/routes`: controller skeleton + opTracker + metrics** — `controller.go` (struct, constructor, Run, handlers, worker loop contract), `optracker.go`, `metrics.go`. Tests for enqueue keying/handler filters + tracker batch-poll behavior.
6. **`internal/routes`: reconcile state machine + node patch helpers** — `reconcile.go`, `nodepatch.go` + the full test matrix (§15). Largest commit; the state machine lands whole because partial machines aren't meaningfully testable.
7. **`internal/routes`: reaper** — `reaper.go` + tests (grace period, chunked batch delete), desired/actual gauges.
8. **Wiring: `register.go` + `main.go` registration** — §6.2/§6.3; overlay-mode no-op log line; fail-fast tests for missing/partial native config.
9. **Deployment manifest + docs** — new release yaml with env block and least-privilege RBAC (§14), README note for `CRUSOE_ROUTING_MODE`.

## 17. What Changes When the Real gRPC Client Lands

The generated clients **already exist**: `gitlab.com/crusoeenergy/schemas` ships `api/island/v2/region/pod_cidr_allocation_management_v2.pb.go` (+ grpc stubs) and region mocks. When SDN endpoint config lands, the swap is:

1. One new file `internal/routes/sdn/grpc.go`: `type grpcClient struct{...}` implementing `PodCIDRAllocationClient` over the schemas-generated client, constructed from `cfg.SDNEndpoint` + mTLS material paths. Its only non-mechanical work is error mapping: gRPC status codes → sentinels (`codes.Unavailable`/`codes.Aborted` → `ErrUnavailable`, `codes.InvalidArgument` → `ErrInvalidArgument`, `codes.NotFound` → `ErrNotFound`), and for `codes.FailedPrecondition` inspect `status.Convert(err).Details()` for `google.rpc.ErrorInfo` with reason `DESTINATION_ALLOCATED_TO_ANOTHER_INTERFACE` → `ErrDestinationConflict` (reason-based, NOT code-based — FAILED_PRECONDITION is also the unclassified bucket).
2. `register.go` step 6 (§6.2): replace `sdn.NewLoggingFakeClient()` with the real constructor (behind the already-defined env vars), plus go.mod dependency on the schemas module.

Nothing else changes: the state machine, tracker, reaper, tests, metrics, and RBAC are client-agnostic by construction. `fake.go` already models the merged contract's semantics, so it remains a truthful test double.

## 18. Open Questions / Risks

- **List pagination** is unspecified; the reaper assumes one `ListPodCIDRAllocations` call returns the full reservation's allocation set. Revisit if reservations grow past a few thousand /24s.
- **Operation retention** is unspecified; the design tolerates missing ops (C6 falls back to List; tracker drops after 3 misses), but a very short retention window turns FAILED creates into "absent → recreate", which is safe (idempotent) but loses the failure detail.
- **Create cap = 1**: a 500-node pool join issues 500 create RPCs (one per reconcile). Fine at current scale; if the cap rises, batching creates across queue keys is a contained change in C8 + tracker (the request shape already supports it).
- **Zero-node bootstrap** (§5.1): resolved — location comes from the cluster object at startup via the `--cluster-name` flag, before any node joins; instance metadata is only a fallback. Residual dependency: the rendered `--cluster-name` must equal the Crusoe cluster resource name (contract note recorded for the addon-controller/clusterlet review).
- **Sustained conflicts = leaked allocation**: the conflict path (C10) self-resolves only if the old VM's delete eventually lands. If it never does (failed VM-delete flow), the node stays tainted until the reaper's grace-gated NIC-mismatch delete fires — worst case `ReaperGrace + ReaperInterval` (~15m default). Acceptable for now; tune via the two knobs if joins are too slow.
- **Batch delete all-or-nothing semantics**: a chunk containing an id from another vpc/location fails the whole chunk `NOT_FOUND`. Should be impossible (ids come from a reservation-scoped List) — handled defensively (§11 step 5) with log + next-pass re-list, no bisection logic.
- **CiliumNode fixture** must be captured from a real CMK native-mode cluster before commit 3; until then a hand-written fixture matching cilium v2 CRD shape is used and flagged in the test file.

## Implementation Status

Branch: `CRUSOE-97212-vpc-native-pod-routing` (commit-by-commit per §16).

| # | Commit | Status |
|---|--------|--------|
| 1 | `internal/routes/sdn`: types, PodCIDRAllocationClient interface, logging fake, gomock | done: a447785 |
| 2 | `internal/routes`: config loading | done: 88b7642 |
| 3 | `internal/routes`: CiliumNode conversion | done: 22bdbee |
| 4 | `internal/routes`: NIC resolution + location sourcing (+ GetClusterByName, mock regen) | done: 92d6aa5 |
| 5 | `internal/routes`: controller skeleton + opTracker + metrics | done: 280239e |
| 6 | `internal/routes`: reconcile state machine + node patch helpers | done: bd5d443 |
| 7 | `internal/routes`: reaper | done: c927cfe |
| 8 | Wiring: register.go + main.go registration | done |
| 9 | Deployment manifest + docs | pending |

### Deviations from the design doc

- **Local tooling only (no source change):** `vendor/` is gitignored in this repo, so builds run in module mode against a go1.26 toolchain; `golangci-lint` is pinned to v1.64.8 (Makefile) and built from source. Neither affects committed code.
- **C1 (mock generation):** `mockgen` is not installed in the environment, so `internal/routes/sdn/mock/client.go` is hand-written in the exact MockGen output style (matching the existing `internal/client/mock/client.go`) rather than tool-generated. The `//go:generate mockgen -source=client.go -destination=mock/client.go` directive is present so it regenerates identically once `mockgen` is available. The generated package name follows the mockgen default for the source dir (`mock_sdn`).
- **C1 (uuid dependency):** `github.com/google/uuid` was already in the module graph (indirect); using it in `fake.go` promoted it to a direct dependency via `go mod tidy`. No new module was added (per ground rule 3).
- **C4 (ListClusters signature):** the design cites `KubernetesClustersApi.ListClusters(ctx, projectID)` (vendored v0.1.68). The module actually resolves to client-go **v0.1.128**, whose `ListClusters(ctx, projectID, *KubernetesClustersApiListClustersOpts)` adds a `ClusterName` server-side filter. `GetClusterByName` passes `ClusterName` as the opt and still filters the result by exact name defensively (list-and-filter behavior unchanged).
- **C4 (struct ordering):** `RouteController`/`nodeState` are introduced in `controller.go` in this commit (only the `cfg`, `apiClient`, `mu`, `state`/`nicID` fields NIC resolution touches) so `nic.go` compiles standalone; the controller-skeleton commit fills in the remaining fields. Unexported helpers are added to `export_test.go` as they are needed so each commit passes the `unused` linter independently. `resolveNIC` and `resolveLocationFromCluster` are exercised through the mock `APIClient`; `internal/client/mock/client.go` gained `GetClusterByName` by hand (mockgen unavailable).
- **C5 (reconcile/reaper stubs):** to keep the controller-skeleton commit building and lint-clean independently, `reconcile.go` and `reaper.go` land here as minimal stubs (`reconcile` returns `(0, nil)`; `reapOnce` is a no-op) that satisfy the worker-loop / goroutine wiring. Their full bodies land in commits 6 and 7 respectively. `nodeState` carries only `nicID` until commit 6 adds the fields the state machine uses (the `unused` linter forbids dead fields per commit). `podsUnroutableTaint` and `ciliumNodeGVR` globals are likewise introduced in the commit that first uses them (6 and 8).
