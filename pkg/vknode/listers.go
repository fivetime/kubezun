package vknode

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// The pod controller resolves a pod's envFrom and valueFrom references through
// listers it is given, and a reference it cannot resolve fails the pod unless
// the pod marked it optional. So those listers have to answer — but it reads
// them one object at a time, by namespace and name, and never registers a
// handler on the informers they come from. That is the whole requirement, and a
// cache is not part of it.
//
// So these read from the API server. What that avoids is the cache: this
// process serves one tenant, and any cache wide enough to answer for every
// namespace that tenant may create is also wide enough to hold every other
// tenant's Secrets — a copy of their credentials, in this tenant's process, for
// a lookup that happens a handful of times per pod.
//
// The cost is a request per reference per pod sync, against a cached read. A
// pod has few references and syncs rarely, and the reads are authorized by the
// API server against this process's own credential, so a pod naming a Secret in
// a namespace this process cannot read gets a refusal rather than the Secret.

// apiSecretLister answers Secret lookups from the API server.
type apiSecretLister struct {
	client kubernetes.Interface
	// namespaces bounds List, which the pod controller does not use for
	// Secrets but which the interface requires.
	namespaces func() []string
}

func (l apiSecretLister) Secrets(namespace string) corev1listers.SecretNamespaceLister {
	return apiSecretNamespaceLister{client: l.client, namespace: namespace}
}

func (l apiSecretLister) List(selector labels.Selector) ([]*corev1.Secret, error) {
	var out []*corev1.Secret
	for _, ns := range l.namespaces() {
		got, err := l.Secrets(ns).List(selector)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

type apiSecretNamespaceLister struct {
	client    kubernetes.Interface
	namespace string
}

func (l apiSecretNamespaceLister) Get(name string) (*corev1.Secret, error) {
	return l.client.CoreV1().Secrets(l.namespace).Get(
		context.Background(), name, metav1.GetOptions{})
}

func (l apiSecretNamespaceLister) List(selector labels.Selector) ([]*corev1.Secret, error) {
	list, err := l.client.CoreV1().Secrets(l.namespace).List(
		context.Background(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	out := make([]*corev1.Secret, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// apiConfigMapLister answers ConfigMap lookups from the API server.
type apiConfigMapLister struct {
	client     kubernetes.Interface
	namespaces func() []string
}

func (l apiConfigMapLister) ConfigMaps(namespace string) corev1listers.ConfigMapNamespaceLister {
	return apiConfigMapNamespaceLister{client: l.client, namespace: namespace}
}

func (l apiConfigMapLister) List(selector labels.Selector) ([]*corev1.ConfigMap, error) {
	var out []*corev1.ConfigMap
	for _, ns := range l.namespaces() {
		got, err := l.ConfigMaps(ns).List(selector)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

type apiConfigMapNamespaceLister struct {
	client    kubernetes.Interface
	namespace string
}

func (l apiConfigMapNamespaceLister) Get(name string) (*corev1.ConfigMap, error) {
	return l.client.CoreV1().ConfigMaps(l.namespace).Get(
		context.Background(), name, metav1.GetOptions{})
}

func (l apiConfigMapNamespaceLister) List(selector labels.Selector) ([]*corev1.ConfigMap, error) {
	list, err := l.client.CoreV1().ConfigMaps(l.namespace).List(
		context.Background(), metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	out := make([]*corev1.ConfigMap, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// secretInformerShim and configMapInformerShim satisfy the informer types the
// pod controller's config asks for while carrying one of the listers above.
//
// Informer() panics rather than returning something empty. The pod controller
// does not call it — verified against node/podcontroller.go, which takes only
// Lister() from these three — and an empty SharedIndexInformer would silently
// never fire, which is the failure that is hard to find later. A panic names
// the reason at the moment the assumption stops holding.
type secretInformerShim struct{ lister corev1listers.SecretLister }

func (s secretInformerShim) Lister() corev1listers.SecretLister { return s.lister }
func (s secretInformerShim) Informer() cache.SharedIndexInformer {
	panic("vknode: the Secret informer has no shared index informer; " +
		"this process reads Secrets on demand so that it never caches another tenant's")
}

type configMapInformerShim struct{ lister corev1listers.ConfigMapLister }

func (s configMapInformerShim) Lister() corev1listers.ConfigMapLister { return s.lister }
func (s configMapInformerShim) Informer() cache.SharedIndexInformer {
	panic("vknode: the ConfigMap informer has no shared index informer; " +
		"this process reads ConfigMaps on demand so that it never caches another tenant's")
}

var (
	_ corev1informers.SecretInformer    = secretInformerShim{}
	_ corev1informers.ConfigMapInformer = configMapInformerShim{}
	_ corev1listers.SecretLister        = apiSecretLister{}
	_ corev1listers.ConfigMapLister     = apiConfigMapLister{}
)
