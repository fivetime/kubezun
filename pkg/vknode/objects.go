package vknode

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// objectReader reads ConfigMaps and Secrets straight from the API server.
//
// No cache. A pod's file volumes are read once, when its capsule is built, so
// there is nothing a cache would serve twice — and the cache that would have to
// exist to answer for any namespace the tenant might create would also hold
// every other tenant's Secrets in this tenant's process, which is a copy of
// their credentials that nothing here needs.
//
// The read is authorized by the API server against this process's own
// credential, and the provider has already checked that the namespace is one it
// serves, so a pod cannot name a Secret outside the tenant and have it fetched.
type objectReader struct {
	client kubernetes.Interface
}

func (r objectReader) ConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error) {
	return r.client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (r objectReader) Secret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	return r.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
}

// Service reads one Service, for the resolver address a capsule is given. Same
// reasoning as above: one read when a capsule is built, nothing to cache.
func (r objectReader) Service(ctx context.Context, namespace, name string) (*corev1.Service, error) {
	return r.client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
}
