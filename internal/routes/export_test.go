package routes

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// This file exposes unexported symbols to the external routes_test package so
// tests can exercise internal helpers without widening the production API.

// CiliumNode is the exported alias of the internal ciliumNode projection, for
// tests.
type CiliumNode = ciliumNode

// CiliumNodeFromUnstructured exposes ciliumNodeFromUnstructured to tests.
func CiliumNodeFromUnstructured(u *unstructured.Unstructured) (*CiliumNode, error) {
	return ciliumNodeFromUnstructured(u)
}
