package routes

import (
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// ErrNotCiliumNode indicates the supplied object is not a CiliumNode
// unstructured value.
var ErrNotCiliumNode = errors.New("object is not a CiliumNode unstructured")

// ciliumNode is the read-only projection of a cilium.io/v2 CiliumNode that the
// controller needs. The controller never writes CiliumNode (KM MR 1314 owns
// deletion), so only the fields relevant to conversion are extracted.
type ciliumNode struct {
	Name              string
	UID               types.UID
	ResourceVersion   string
	DeletionTimestamp *metav1.Time
	PodCIDRs          []string // spec.ipam.podCIDRs
}

// ciliumNodeFromUnstructured extracts the projection above from an unstructured
// CiliumNode. A missing spec.ipam.podCIDRs is NOT an error (returns an empty
// slice) — cilium fills it asynchronously.
func ciliumNodeFromUnstructured(u *unstructured.Unstructured) (*ciliumNode, error) {
	if u == nil || u.Object == nil {
		return nil, ErrNotCiliumNode
	}
	if kind := u.GetKind(); kind != "" && kind != "CiliumNode" {
		return nil, fmt.Errorf("%w: kind=%q", ErrNotCiliumNode, kind)
	}

	cn := &ciliumNode{
		Name:              u.GetName(),
		UID:               u.GetUID(),
		ResourceVersion:   u.GetResourceVersion(),
		DeletionTimestamp: u.GetDeletionTimestamp(),
	}

	cidrs, err := podCIDRsFromUnstructured(u)
	if err != nil {
		return nil, err
	}
	cn.PodCIDRs = cidrs

	return cn, nil
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
