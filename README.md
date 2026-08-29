# Crusoe Cloud Controller Manager (CCM)

This repository defines the official Cloud Controller Manager (CCM) for use with [Crusoe Cloud](https://crusoecloud.com/), the world's first carbon-reducing, low-cost GPU cloud platform.

## Getting Started

Please follow the [Helm installation instructions](https://github.com/crusoecloud/crusoe-cloud-controller-manager-helm-charts) to install the CCM.

## VPC-native pod routing

In addition to the standard cloud-provider controllers, the CCM ships an
optional `crusoe-route-controller` for VPC-native (non-overlay) pod routing. It
is gated entirely by the `CRUSOE_ROUTING_MODE` environment variable:

- `overlay` (default, or unset): the controller is not started — one log line,
  no effect. This is the behavior for existing clusters.
- `native`: the controller programs one SDN pod CIDR allocation per node (from
  the node's cilium-assigned `/24`) and removes the kubelet-applied
  `crusoe.ai/pods-unroutable:NoSchedule` taint once the allocation is ready.

In native mode the deployment must also set `CRUSOE_VPC_ID` and
`CRUSOE_VPC_PREFIX_RESERVATION_ID`. These three routing variables are all-or-none:
a partial set fails CCM startup loudly (it indicates misrendered config). The
cluster location and SDN project id are derived from platform metadata, so no
further environment is required; `--cluster-name` must equal the Crusoe cluster
resource name (it is used to resolve the cluster's location).

See [`docs/designs/vpc-native-pod-routing-ccm.md`](docs/designs/vpc-native-pod-routing-ccm.md)
for the full design, including the deployment env block and the least-privilege
RBAC the route controller needs (§14). In v2 the CCM Deployment is rendered by
addon-controller, which owns the env/RBAC manifests.