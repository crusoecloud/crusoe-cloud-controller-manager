package routes

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ErrNotCiliumNode indicates the supplied object is not a CiliumNode
// unstructured value.
var ErrNotCiliumNode = errors.New("object is not a CiliumNode unstructured")

// ciliumNode is the read-only projection of a cilium.io/v2 CiliumNode that the
// PodCIDR mirror needs (section 7): just the name and the pod cidrs.
type ciliumNode struct {
	Name     string
	PodCIDRs []string // spec.ipam.podCIDRs
}

// ciliumNodeFromUnstructured extracts the projection above from an unstructured
// CiliumNode. A missing spec.ipam.podCIDRs is NOT an error (returns an empty
// slice), cilium fills it asynchronously.
func ciliumNodeFromUnstructured(u *unstructured.Unstructured) (*ciliumNode, error) {
	if u == nil || u.Object == nil {
		return nil, ErrNotCiliumNode
	}
	if kind := u.GetKind(); kind != "" && kind != "CiliumNode" {
		return nil, fmt.Errorf("%w: kind=%q", ErrNotCiliumNode, kind)
	}

	cidrs, err := podCIDRsFromUnstructured(u)
	if err != nil {
		return nil, err
	}

	return &ciliumNode{Name: u.GetName(), PodCIDRs: cidrs}, nil
}

// podCIDRsFromUnstructured reads spec.ipam.podCIDRs. Missing intermediate fields
// yield an empty slice (not an error); a present-but-wrong-typed field is an
// error.
func podCIDRsFromUnstructured(u *unstructured.Unstructured) ([]string, error) {
	raw, found, err := unstructured.NestedStringSlice(u.Object, "spec", "ipam", "podCIDRs")
	if err != nil {
		return nil, fmt.Errorf("failed to read spec.ipam.podCIDRs for CiliumNode %s: %w", u.GetName(), err)
	}
	if !found {
		return nil, nil
	}

	return raw, nil
}
