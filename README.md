# Crusoe Cloud Controller Manager (CCM)

This repository defines the official Cloud Controller Manager (CCM) for use with [Crusoe Cloud](https://crusoecloud.com/), the world's first carbon-reducing, low-cost GPU cloud platform.

## Getting Started

Please follow the [Helm installation instructions](https://github.com/crusoecloud/crusoe-cloud-controller-manager-helm-charts) to install the CCM.

## Kubernetes Version Support

This project supports multiple Kubernetes versions through separate branches and Docker image tags.

### Available Kubernetes Versions

The CCM is built and tested against the following Kubernetes versions:

- 1.30.x - `release-1.30` branch
- 1.31.x - `release-1.31` branch
- 1.32.x - `release-1.32` branch
- 1.33.x - `release-1.33` branch

### Docker Images

Docker images are tagged with both the CCM version and the Kubernetes version:

```
registry.gitlab.com/crusoeenergy/island/external/crusoe-cloud-controller-manager:v0.1.1-k8s-1.30
registry.gitlab.com/crusoeenergy/island/external/crusoe-cloud-controller-manager:v0.1.1-k8s-1.31
```

### Creating a New K8s Version Branch

To create a branch for a new Kubernetes version:

```bash
# Usage: ./scripts/create_k8s_branch.sh <k8s-version> [base-branch]
./scripts/create_k8s_branch.sh 1.34 main
```

This script will:
1. Create a new branch named `release-1.34`
2. Update the go.mod file with the appropriate Kubernetes library versions
3. Run `go mod tidy` to update dependencies

### Building for a Specific K8s Version

To build for a specific Kubernetes version:

```bash
# Build using the K8S_VERSION environment variable
K8S_VERSION=1.30 make build

# Or for Docker builds
docker build --build-arg K8S_VERSION=1.30 -t crusoe-cloud-controller-manager:v0.1.1-k8s-1.30 .