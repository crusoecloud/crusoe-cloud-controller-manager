#!/bin/bash
# Script to create a new branch for a specific Kubernetes version

set -e

# Check if K8s version is provided
if [ $# -lt 1 ]; then
    echo "Usage: $0 <k8s-version> [base-branch]"
    echo "Example: $0 1.35 main"
    exit 1
fi

K8S_VERSION=$1
BASE_BRANCH=${2:-main}

# Validate K8s version format
if ! [[ $K8S_VERSION =~ ^[0-9]+\.[0-9]+$ ]]; then
    echo "Error: K8s version must be in format X.Y (e.g., 1.31)"
    exit 1
fi

BRANCH_NAME="release-$K8S_VERSION"

# Check if branch already exists
if git rev-parse --verify $BRANCH_NAME &>/dev/null; then
    echo "Branch $BRANCH_NAME already exists!"
    exit 1
fi

# Create new branch
echo "Creating branch $BRANCH_NAME from $BASE_BRANCH..."
git checkout $BASE_BRANCH
git pull
git checkout -b $BRANCH_NAME

# Update go.mod for the specific K8s version
echo "Updating go.mod for Kubernetes $K8S_VERSION..."

# Fetch the latest library version for the specified K8s version from Go proxy
echo "Fetching latest library version for Kubernetes $K8S_VERSION from Go proxy..."

# Use the Go proxy to determine the available versions
K8S_MINOR=${K8S_VERSION#*.}

# Query available versions from the Go proxy
AVAILABLE_VERSIONS=$(curl -s https://proxy.golang.org/k8s.io/api/@v/list | grep "^v0\.$K8S_MINOR\." | sort -V)

if [ -n "$AVAILABLE_VERSIONS" ]; then
    # Get the latest version
    K8S_LIB_VERSION=${AVAILABLE_VERSIONS##*$'\n'}
    K8S_LIB_VERSION=${K8S_LIB_VERSION#v}
    echo "Found library version: $K8S_LIB_VERSION for Kubernetes $K8S_VERSION"
else
    # Fallback to a default version format if no versions found
    K8S_LIB_VERSION="0.$K8S_MINOR.0"
    echo "Warning: Could not find library version for Kubernetes $K8S_VERSION from Go proxy."
    echo "Using default version: $K8S_LIB_VERSION"
    
    # Verify if this library version exists
    VERIFY_LIB=$(curl -s https://pkg.go.dev/k8s.io/api@v$K8S_LIB_VERSION -o /dev/null -w "%{http_code}")
    
    if [ "$VERIFY_LIB" != "200" ]; then
        echo "Warning: Library version v$K8S_LIB_VERSION not found in pkg.go.dev."
        echo "Please manually update the go.mod file with the appropriate library versions."
        exit 1
    fi
fi

# Display current versions in go.mod before updating
echo "Current versions in go.mod:"
grep -E "k8s.io/(api|apimachinery|client-go|cloud-provider|component-base|component-helpers|controller-manager)" go.mod

# Update go.mod file with the appropriate K8s library versions
# Using macOS compatible sed commands
sed -i '' "s/k8s.io\/api v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*/k8s.io\/api v$K8S_LIB_VERSION/g" go.mod
sed -i '' "s/k8s.io\/apimachinery v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*/k8s.io\/apimachinery v$K8S_LIB_VERSION/g" go.mod
sed -i '' "s/k8s.io\/client-go v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*/k8s.io\/client-go v$K8S_LIB_VERSION/g" go.mod
sed -i '' "s/k8s.io\/cloud-provider v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*/k8s.io\/cloud-provider v$K8S_LIB_VERSION/g" go.mod
sed -i '' "s/k8s.io\/component-base v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*/k8s.io\/component-base v$K8S_LIB_VERSION/g" go.mod
sed -i '' "s/k8s.io\/component-helpers v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*/k8s.io\/component-helpers v$K8S_LIB_VERSION/g" go.mod
sed -i '' "s/k8s.io\/controller-manager v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*/k8s.io\/controller-manager v$K8S_LIB_VERSION/g" go.mod

# Display updated versions in go.mod after updating
echo "Updated versions in go.mod:"
grep -E "k8s.io/(api|apimachinery|client-go|cloud-provider|component-base|component-helpers|controller-manager)" go.mod

# Run go mod tidy to update dependencies
echo "Running go mod tidy..."
go mod tidy

echo "Branch $BRANCH_NAME created and updated for Kubernetes $K8S_VERSION"
echo "Next steps:"
echo "1. Test the build: make build"
echo "2. Fix any compatibility issues"
echo "3. Commit changes: git commit -am 'Update dependencies for K8s $K8S_VERSION'"
echo "4. Push branch: git push -u origin $BRANCH_NAME"
