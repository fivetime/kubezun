package vknode

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	discoveryv1listers "k8s.io/client-go/listers/discovery/v1"
	networkingv1listers "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
)

// ScopedFactories watches a set of kinds in exactly the namespaces this
// process serves, one informer factory per namespace, started and stopped as
// namespaces come and go (DESIGN §2.2).
//
// Why not one cluster-wide factory: the process caches every tenant's objects
// to serve a few — measured at 6× with two tenants — and under sharding that
// multiplies by K, cancelling what sharding is for.
//
// Why not per-namespace reflectors into one shared indexer: a Reflector's
// initial sync calls Replace, whose contract is "this is everything", and it
// would erase every other namespace's objects each time one namespace listed.
// Separate factories keep each namespace's store its own; the aggregation is
// read-side only, so removing a namespace is removing its factory — there is
// nothing to purge, and a departed tenant's objects cannot linger in peer
// sets because the read path no longer includes them.
type ScopedFactories struct {
	client kubernetes.Interface
	resync time.Duration

	mu        sync.Mutex
	ctx       context.Context
	synced    bool // latched: see HasSynced
	factories map[string]*scopedFactory
	// handlers are registered on every current and future factory's informers,
	// so a controller subscribes once and hears about namespaces that appear
	// later.
	handlers []scopedHandler
}

type scopedFactory struct {
	factory  informers.SharedInformerFactory
	cancel   context.CancelFunc
	services cache.SharedIndexInformer
	slices   cache.SharedIndexInformer
	pods     cache.SharedIndexInformer
	policies cache.SharedIndexInformer
	ingress  cache.SharedIndexInformer
	claims   cache.SharedIndexInformer
}

// informerOf maps a kind name to the informer, one place for registration,
// sync and the listers to agree on.
func (f *scopedFactory) informerOf(kind string) cache.SharedIndexInformer {
	switch kind {
	case "services":
		return f.services
	case "endpointslices":
		return f.slices
	case "pods":
		return f.pods
	case "networkpolicies":
		return f.policies
	case "ingresses":
		return f.ingress
	case "persistentvolumeclaims":
		return f.claims
	}
	return nil
}

// scopedKinds is every kind a factory watches; sync covers them all.
var scopedKinds = []string{"services", "endpointslices", "pods",
	"networkpolicies", "ingresses", "persistentvolumeclaims"}

type scopedHandler struct {
	kind    string // "services" | "endpointslices"
	handler cache.ResourceEventHandler
}

// NewScopedFactories builds the set; namespaces are added via Track, normally
// wired to Namespaces.OnChange plus its current List.
func NewScopedFactories(client kubernetes.Interface, resync time.Duration) *ScopedFactories {
	return &ScopedFactories{
		client:    client,
		resync:    resync,
		factories: map[string]*scopedFactory{},
	}
}

// Start begins serving; namespaces tracked before Start are started now.
func (s *ScopedFactories) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ctx = ctx
	for ns, f := range s.factories {
		if f.cancel == nil {
			s.startLocked(ns, f)
		}
	}
}

// Track adds and removes namespaces. Removal stops the factory and drops its
// store from the read path in one move.
func (s *ScopedFactories) Track(added, removed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ns := range removed {
		if f, ok := s.factories[ns]; ok {
			if f.cancel != nil {
				f.cancel()
			}
			f.factory.Shutdown()
			delete(s.factories, ns)
		}
	}
	for _, ns := range added {
		if _, ok := s.factories[ns]; ok {
			continue
		}
		factory := informers.NewSharedInformerFactoryWithOptions(
			s.client, s.resync, informers.WithNamespace(ns))
		f := &scopedFactory{
			factory:  factory,
			services: factory.Core().V1().Services().Informer(),
			slices:   factory.Discovery().V1().EndpointSlices().Informer(),
			pods:     factory.Core().V1().Pods().Informer(),
			policies: factory.Networking().V1().NetworkPolicies().Informer(),
			ingress:  factory.Networking().V1().Ingresses().Informer(),
			claims:   factory.Core().V1().PersistentVolumeClaims().Informer(),
		}
		// Handlers registered before the factory starts, so the initial list
		// is delivered as Add events the same way a shared informer would.
		for _, h := range s.handlers {
			_ = s.registerLocked(f, h)
		}
		s.factories[ns] = f
		if s.ctx != nil {
			s.startLocked(ns, f)
		}
	}
}

func (s *ScopedFactories) startLocked(_ string, f *scopedFactory) {
	ctx, cancel := context.WithCancel(s.ctx)
	f.cancel = cancel
	f.factory.Start(ctx.Done())
}

func (s *ScopedFactories) registerLocked(f *scopedFactory, h scopedHandler) error {
	inf := f.informerOf(h.kind)
	if inf == nil {
		return fmt.Errorf("unknown kind %q", h.kind)
	}
	_, err := inf.AddEventHandler(h.handler)
	return err
}

// OnServices and OnEndpointSlices subscribe a handler to every namespace,
// current and future.
func (s *ScopedFactories) OnServices(h cache.ResourceEventHandler) { s.subscribe("services", h) }
func (s *ScopedFactories) OnEndpointSlices(h cache.ResourceEventHandler) {
	s.subscribe("endpointslices", h)
}
func (s *ScopedFactories) OnPods(h cache.ResourceEventHandler)     { s.subscribe("pods", h) }
func (s *ScopedFactories) OnPolicies(h cache.ResourceEventHandler) { s.subscribe("networkpolicies", h) }
func (s *ScopedFactories) OnIngresses(h cache.ResourceEventHandler) {
	s.subscribe("ingresses", h)
}
func (s *ScopedFactories) OnClaims(h cache.ResourceEventHandler) {
	s.subscribe("persistentvolumeclaims", h)
}

func (s *ScopedFactories) subscribe(kind string, h cache.ResourceEventHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers = append(s.handlers, scopedHandler{kind: kind, handler: h})
	for _, f := range s.factories {
		_ = s.registerLocked(f, scopedHandler{kind: kind, handler: h})
	}
}

// HasSynced reports whether every CURRENTLY tracked namespace has finished its
// first list. ⚠️ Deliberately a statement about the present set: a namespace
// that joins later starts unsynced without flipping this back to false — a
// controller that waited once must not stall because a tenant onboarded. The
// window in which a new namespace's objects are not yet listed is the same
// window a freshly created object is not yet watched: ordinary staleness, not
// wrongness, and events close it.
func (s *ScopedFactories) HasSynced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.synced {
		// ⚠️ Latched. A namespace joining later starts unsynced, and without
		// the latch this answer would flip back to false — stalling every
		// controller that gates work on it, every time a tenant onboards. The
		// window in which the new namespace's objects are not yet listed is
		// ordinary staleness, closed by events, not unreadiness.
		return true
	}
	for _, f := range s.factories {
		if f.cancel == nil {
			continue // not started yet; Start's caller gates on that
		}
		for _, kind := range scopedKinds {
			if !f.informerOf(kind).HasSynced() {
				return false
			}
		}
	}
	s.synced = true
	return true
}

// snapshot returns the current factories, for the read path.
func (s *ScopedFactories) snapshot() map[string]*scopedFactory {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*scopedFactory, len(s.factories))
	for ns, f := range s.factories {
		out[ns] = f
	}
	return out
}

// ServiceLister returns a lister spanning the tracked namespaces. It is the
// standard interface, so consumers do not know they are reading a fan-out.
func (s *ScopedFactories) ServiceLister() corev1listers.ServiceLister {
	return scopedServiceLister{s}
}

// EndpointSliceLister is the discovery counterpart.
func (s *ScopedFactories) EndpointSliceLister() discoveryv1listers.EndpointSliceLister {
	return scopedSliceLister{s}
}

type scopedServiceLister struct{ s *ScopedFactories }

func (l scopedServiceLister) List(selector labels.Selector) ([]*corev1.Service, error) {
	var out []*corev1.Service
	for _, f := range l.s.snapshot() {
		part, err := corev1listers.NewServiceLister(f.services.GetIndexer()).List(selector)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (l scopedServiceLister) Services(namespace string) corev1listers.ServiceNamespaceLister {
	if f, ok := l.s.snapshot()[namespace]; ok {
		return corev1listers.NewServiceLister(f.services.GetIndexer()).Services(namespace)
	}
	// An untracked namespace reads as empty, which is also what a cluster-wide
	// lister answers for a namespace with nothing in it. Refusing here would
	// make every caller distinguish "no such tenant" from "no Services", and
	// the callers that care already gate on ServesNamespace.
	return emptyServiceLister{namespace}
}

type emptyServiceLister struct{ namespace string }

func (e emptyServiceLister) List(labels.Selector) ([]*corev1.Service, error) { return nil, nil }
func (e emptyServiceLister) Get(name string) (*corev1.Service, error) {
	return nil, apierrNotFound("service", e.namespace, name)
}

type scopedSliceLister struct{ s *ScopedFactories }

func (l scopedSliceLister) List(selector labels.Selector) ([]*discoveryv1.EndpointSlice, error) {
	var out []*discoveryv1.EndpointSlice
	for _, f := range l.s.snapshot() {
		part, err := discoveryv1listers.NewEndpointSliceLister(f.slices.GetIndexer()).List(selector)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (l scopedSliceLister) EndpointSlices(namespace string) discoveryv1listers.EndpointSliceNamespaceLister {
	if f, ok := l.s.snapshot()[namespace]; ok {
		return discoveryv1listers.NewEndpointSliceLister(f.slices.GetIndexer()).EndpointSlices(namespace)
	}
	return emptySliceLister{namespace}
}

type emptySliceLister struct{ namespace string }

func (e emptySliceLister) List(labels.Selector) ([]*discoveryv1.EndpointSlice, error) {
	return nil, nil
}
func (e emptySliceLister) Get(name string) (*discoveryv1.EndpointSlice, error) {
	return nil, apierrNotFound("endpointslice", e.namespace, name)
}

// apierrNotFound builds the same error a real lister returns, so callers'
// IsNotFound checks keep working.
func apierrNotFound(resource, namespace, name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Resource: resource + "s"},
		namespace+"/"+name)
}

// PodLister spans the tracked namespaces. ⚠️ This is the shard-scoped pod view
// DESIGN §2.2 requires for NetworkPolicy peers — never narrower: a policy's
// peers are pods wherever they run within the tenant, and this set contains
// every namespace the process serves.
func (s *ScopedFactories) PodLister() corev1listers.PodLister { return scopedPodLister{s} }

// NetworkPolicyLister spans the tracked namespaces.
func (s *ScopedFactories) NetworkPolicyLister() networkingv1listers.NetworkPolicyLister {
	return scopedPolicyLister{s}
}

// IngressLister spans the tracked namespaces.
func (s *ScopedFactories) IngressLister() networkingv1listers.IngressLister {
	return scopedIngressLister{s}
}

// ClaimLister spans the tracked namespaces.
func (s *ScopedFactories) ClaimLister() corev1listers.PersistentVolumeClaimLister {
	return scopedClaimLister{s}
}

type scopedPodLister struct{ s *ScopedFactories }

func (l scopedPodLister) List(sel labels.Selector) ([]*corev1.Pod, error) {
	var out []*corev1.Pod
	for _, f := range l.s.snapshot() {
		part, err := corev1listers.NewPodLister(f.pods.GetIndexer()).List(sel)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (l scopedPodLister) Pods(ns string) corev1listers.PodNamespaceLister {
	if f, ok := l.s.snapshot()[ns]; ok {
		return corev1listers.NewPodLister(f.pods.GetIndexer()).Pods(ns)
	}
	return emptyPodLister{ns}
}

type emptyPodLister struct{ namespace string }

func (e emptyPodLister) List(labels.Selector) ([]*corev1.Pod, error) { return nil, nil }
func (e emptyPodLister) Get(name string) (*corev1.Pod, error) {
	return nil, apierrNotFound("pod", e.namespace, name)
}

type scopedPolicyLister struct{ s *ScopedFactories }

func (l scopedPolicyLister) List(sel labels.Selector) ([]*networkingv1.NetworkPolicy, error) {
	var out []*networkingv1.NetworkPolicy
	for _, f := range l.s.snapshot() {
		part, err := networkingv1listers.NewNetworkPolicyLister(f.policies.GetIndexer()).List(sel)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (l scopedPolicyLister) NetworkPolicies(ns string) networkingv1listers.NetworkPolicyNamespaceLister {
	if f, ok := l.s.snapshot()[ns]; ok {
		return networkingv1listers.NewNetworkPolicyLister(f.policies.GetIndexer()).NetworkPolicies(ns)
	}
	return emptyPolicyLister{ns}
}

type emptyPolicyLister struct{ namespace string }

func (e emptyPolicyLister) List(labels.Selector) ([]*networkingv1.NetworkPolicy, error) {
	return nil, nil
}
func (e emptyPolicyLister) Get(name string) (*networkingv1.NetworkPolicy, error) {
	return nil, apierrNotFound("networkpolicy", e.namespace, name)
}

type scopedIngressLister struct{ s *ScopedFactories }

func (l scopedIngressLister) List(sel labels.Selector) ([]*networkingv1.Ingress, error) {
	var out []*networkingv1.Ingress
	for _, f := range l.s.snapshot() {
		part, err := networkingv1listers.NewIngressLister(f.ingress.GetIndexer()).List(sel)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (l scopedIngressLister) Ingresses(ns string) networkingv1listers.IngressNamespaceLister {
	if f, ok := l.s.snapshot()[ns]; ok {
		return networkingv1listers.NewIngressLister(f.ingress.GetIndexer()).Ingresses(ns)
	}
	return emptyIngressLister{ns}
}

type emptyIngressLister struct{ namespace string }

func (e emptyIngressLister) List(labels.Selector) ([]*networkingv1.Ingress, error) { return nil, nil }
func (e emptyIngressLister) Get(name string) (*networkingv1.Ingress, error) {
	return nil, apierrNotFound("ingress", e.namespace, name)
}

type scopedClaimLister struct{ s *ScopedFactories }

func (l scopedClaimLister) List(sel labels.Selector) ([]*corev1.PersistentVolumeClaim, error) {
	var out []*corev1.PersistentVolumeClaim
	for _, f := range l.s.snapshot() {
		part, err := corev1listers.NewPersistentVolumeClaimLister(f.claims.GetIndexer()).List(sel)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (l scopedClaimLister) PersistentVolumeClaims(ns string) corev1listers.PersistentVolumeClaimNamespaceLister {
	if f, ok := l.s.snapshot()[ns]; ok {
		return corev1listers.NewPersistentVolumeClaimLister(f.claims.GetIndexer()).PersistentVolumeClaims(ns)
	}
	return emptyClaimLister{ns}
}

type emptyClaimLister struct{ namespace string }

func (e emptyClaimLister) List(labels.Selector) ([]*corev1.PersistentVolumeClaim, error) {
	return nil, nil
}
func (e emptyClaimLister) Get(name string) (*corev1.PersistentVolumeClaim, error) {
	return nil, apierrNotFound("persistentvolumeclaim", e.namespace, name)
}
