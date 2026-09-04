# Crusoe Cloud Controller Manager (CCM)

This repository defines the official Cloud Controller Manager (CCM) for use with [Crusoe Cloud](https://crusoecloud.com/), the world's first carbon-reducing, low-cost GPU cloud platform.

## Getting Started

Please follow the [Helm installation instructions](https://github.com/crusoecloud/crusoe-cloud-controller-manager-helm-charts) to install the CCM.

## VPC-native pod routing

For VPC-native (non-overlay) pod routing the CCM uses the **upstream
cloud-provider route controller** (`node-route-controller`), backed by a Crusoe
`cloudprovider.Routes` implementation that programs one SDN pod CIDR allocation
per node, plus a small `crusoe-podcidr-mirror` controller that copies each
node's cilium-assigned v4 `/24` from `CiliumNode.spec.ipam.podCIDRs` into
`node.spec.podCIDR`/`podCIDRs` (write-once).

Everything is gated by the `CRUSOE_ROUTING_MODE` environment variable:

- `overlay` (default, or unset): `Cloud.Routes()` reports no route support, so
  the upstream route controller self-disables, and the mirror is not started.
  This is the behavior for existing clusters, no flags required.
- `native`: routes are reconciled every `--route-reconciliation-period`
  (default 10s). When a node first appears the mirror sets the
  `NodeNetworkUnavailable` condition to `True`; the kube-controller-manager
  node-lifecycle controller translates it to the standard
  `node.kubernetes.io/network-unavailable:NoSchedule` taint, and the route
  controller flips the condition to `False` once the node's route is
  programmed, which lifts the taint. Nothing else may write that condition:
  the cilium operator must run with `--set-cilium-is-up-condition=false`
  (Helm `operator.setNodeNetworkStatus: false`), otherwise it clears the
  condition as soon as the agent is up and pods schedule before the route
  exists.

In native mode the deployment must also set `CRUSOE_VPC_ID` and
`CRUSOE_VPC_PREFIX_RESERVATION_IDS` (comma-separated reservation ids in creation
order, one at cluster create, more after a pod-range expansion). These three
routing variables are all-or-none: a partial set fails CCM startup loudly (it
indicates misrendered config). The SDN project id comes from `CRUSOE_PROJECT_ID`
and the cluster location is resolved at startup from the cluster object, then
mapped to the SDN's internal location name via its `ListLocations` RPC (the
region coordinator accepts internal names only; an unknown location is fatal);
`--cluster-name` must carry the Crusoe cluster id (UUID), which the deployment
already passes.

In native mode `--cluster-cidr` is optional: when unset, the CCM fetches the
VPC network's CIDR at startup and uses that (an explicitly set flag wins; a
failed lookup is fatal). The flag only scopes which stale routes the upstream
controller may delete, and VPC prefix reservations are carved from the VPC
range, so the VPC CIDR always covers them. It never needs updating, in
particular not when a pod range is added after cluster creation (a second
cilium cluster-pool CIDR): that expansion is handled by appending the new
reservation id to `CRUSOE_VPC_PREFIX_RESERVATION_IDS`.
`--configure-cloud-routes` is left at its default (`true`);
`--allocate-node-cidrs` must not be set (cilium cluster-pool IPAM owns pod CIDR
allocation).

The SDN gRPC endpoint is configured via:

- `REGION_ENDPOINT`, the region coordinator gRPC address as `host` or
  `host:port`. This is the value addon-controller renders from the
  cmk-addon-catalog ConfigMap's `regionEndpoint` key, a bare host, so a value
  without a port gets the default region port `9090` appended.
- `CRUSOE_SDN_USE_FAKE`, set to `true` to run an **in-memory logging fake**
  instead of dialing an endpoint (local development only: SDN state lives in
  the process and is not persisted).
- Native mode requires **exactly one** of those two. Neither set, or both set,
  fails CCM startup loudly; there is no silent fallback to the fake.
- `CRUSOE_SDN_CERT_FILE`, `CRUSOE_SDN_KEY_FILE`, `CRUSOE_SDN_CA_FILE`, client
  mTLS material. All-or-none, and they require `REGION_ENDPOINT`. Endpoint
  set with the cert trio absent uses a plaintext connection (local dev); with
  the full trio it uses mTLS.

Building the CCM pulls private `gitlab.com/crusoeenergy/*` modules, so set
`GOPRIVATE=gitlab.com/crusoeenergy/*` (or the equivalent git/netrc auth) when
building from source.

See [`docs/designs/vpc-native-pod-routing-ccm.md`](docs/designs/vpc-native-pod-routing-ccm.md)
for the full design, including the flags contract and the RBAC the controllers
need (section 14). In v2 the CCM Deployment is rendered by addon-controller, which
owns the flag/env/RBAC manifests.
