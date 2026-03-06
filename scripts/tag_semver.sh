#!/usr/bin/env bash
set -e

MAJOR_VERSION=$1
MINOR_VERSION=$2
TAG_PREFIX=$3

# Extract client-go version from go.mod
CLIENT_GO_VERSION=$(grep "k8s.io/client-go" go.mod | head -1 | grep -o 'v[0-9]\+\.[0-9]\+\.[0-9]\+' | sed 's/v//')
CLIENT_GO_MINOR=$(echo "$CLIENT_GO_VERSION" | cut -d. -f2)

echo "Detected client-go version: $CLIENT_GO_VERSION (minor: $CLIENT_GO_MINOR)"

# find the latest tag
NEW_VERSION="${TAG_PREFIX}v${MAJOR_VERSION}.${MINOR_VERSION}.0-k8s${CLIENT_GO_MINOR}"
git fetch -q --tags --prune --prune-tags
tags=$(git tag -l ${TAG_PREFIX}v${MAJOR_VERSION}.${MINOR_VERSION}.*-k8s${CLIENT_GO_MINOR} --sort=-version:refname)
if [[ ! -z "$tags" ]]; then
  arr=(${tags})
  for val in ${arr[@]}; do
    if [[ "$val" =~ ^${TAG_PREFIX}v${MAJOR_VERSION}+\.${MINOR_VERSION}\.[0-9]+-k8s${CLIENT_GO_MINOR}$ ]]; then
      prev_build=$(echo ${val} | cut -d. -f3 | cut -d- -f1)
      new_build=$((prev_build+1))
      NEW_VERSION="${TAG_PREFIX}v${MAJOR_VERSION}.${MINOR_VERSION}.${new_build}-k8s${CLIENT_GO_MINOR}"
      break
    fi
  done
fi

echo "Version for this commit: ${NEW_VERSION}"
echo "RELEASE_VERSION=${NEW_VERSION}" >> variables.env
# Also output the client-go minor version for potential use in the workflow
echo "K8S_MINOR_VERSION=${CLIENT_GO_MINOR}" >> variables.env