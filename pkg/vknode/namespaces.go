package vknode

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Namespaces tracks which namespaces this process serves.
//
// The set is a tenant's namespaces, and a tenant creates them while this is
// running, so it is watched rather than configured. Configured, a namespace made
// after startup has no compute at all and says nothing about why.
//
// It is watched by label rather than derived from the name. The gateway writes
// the label on every namespace it makes for a tenant and refuses a write that
// changes it, so it is as hard to forge as the name — and unlike a name prefix
// it can be handed to the API server as a selector, which is what keeps this
// watch from returning every namespace in the cluster. Deriving it instead would
// put the gateway's rule (an id of a fixed width, then a separator) in this
// codebase, where it would go stale silently and the thing that went stale is an
// authorization boundary.
type Namespaces struct {
	informer cache.SharedIndexInformer

	// tenantLabel names the label whose value identifies the tenant. Empty
	// leaves TenantOf answering "unknown", which is what a deployment serving
	// one tenant per process needs.
	tenantLabel string

	mu sync.RWMutex
	// current maps each served namespace to its tenant, or to the empty string
	// when no tenant label is configured or the namespace carries none.
	// ⚠️ The tenant is recorded here rather than looked up on demand because
	// the answer decides which OpenStack credential a pod's capsule is created
	// with (DESIGN §4.6). A lookup that can fail mid-flight would have to
	// choose between refusing and guessing.
	current map[string]string

	// observers are told when the set changes, so informers scoped to a
	// namespace can be started and stopped with it.
	observers []func(added, removed []string)
}

// NewNamespaces watches the namespaces carrying the given label selector.
func NewNamespaces(client kubernetes.Interface, selector string) *Namespaces {
	return NewNamespacesWithTenantLabel(client, selector, "")
}

// NewNamespacesWithTenantLabel also records, for each namespace, the value of
// the given label — the tenant a namespace belongs to, which decides whose
// OpenStack credential its pods are created with.
func NewNamespacesWithTenantLabel(client kubernetes.Interface, selector, tenantLabel string) *Namespaces {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = selector
		}))

	n := &Namespaces{
		informer:    factory.Core().V1().Namespaces().Informer(),
		tenantLabel: tenantLabel,
		current:     map[string]string{},
	}

	n.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { n.add(obj) },
		// No UpdateFunc: a namespace that stops matching the selector is
		// delivered as a delete, and one that starts matching as an add.
		DeleteFunc: func(obj any) { n.remove(obj) },
	})
	return n
}

func (n *Namespaces) add(obj any) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return
	}
	var tenant string
	if n.tenantLabel != "" {
		tenant = ns.Labels[n.tenantLabel]
	}
	n.mu.Lock()
	if known, exists := n.current[ns.Name]; exists && known == tenant {
		n.mu.Unlock()
		return
	}
	n.current[ns.Name] = tenant
	observers := append([]func([]string, []string){}, n.observers...)
	n.mu.Unlock()

	for _, o := range observers {
		o([]string{ns.Name}, nil)
	}
}

func (n *Namespaces) remove(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		return
	}
	n.mu.Lock()
	if _, exists := n.current[ns.Name]; !exists {
		n.mu.Unlock()
		return
	}
	delete(n.current, ns.Name)
	observers := append([]func([]string, []string){}, n.observers...)
	n.mu.Unlock()

	for _, o := range observers {
		o(nil, []string{ns.Name})
	}
}

// Serves reports whether a namespace is one of this tenant's.
//
// ⚠️ This is the authorization boundary (DESIGN §4), so it fails closed: before
// the watch has synced the set is empty and everything is refused. Answering
// "yes" while the set is unknown would run another tenant's pod in this tenant's
// project, which is the one outcome that cannot be undone by a later correction.
func (n *Namespaces) Serves(namespace string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	_, ok := n.current[namespace]
	return ok
}

// TenantOf says which tenant a namespace belongs to, and whether the namespace
// is served at all.
//
// ⚠️ Fails closed for the same reason Serves does: an unserved namespace answers
// false, and a caller that treats "unknown tenant" as "use the default one"
// would create a pod's capsule in whichever project happened to be configured.
func (n *Namespaces) TenantOf(namespace string) (string, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	tenant, ok := n.current[namespace]
	return tenant, ok
}

// List returns the namespaces currently served.
func (n *Namespaces) List() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.current))
	for ns := range n.current {
		out = append(out, ns)
	}
	return out
}

// OnChange registers an observer, and immediately reports what is already
// known so a late registration does not miss the namespaces that exist.
func (n *Namespaces) OnChange(f func(added, removed []string)) {
	n.mu.Lock()
	n.observers = append(n.observers, f)
	existing := make([]string, 0, len(n.current))
	for ns := range n.current {
		existing = append(existing, ns)
	}
	n.mu.Unlock()

	if len(existing) > 0 {
		f(existing, nil)
	}
}

// syncTimeout bounds the first sync. Beyond it the caller is told, and the
// watch keeps trying in the background: whatever is wrong is usually a
// permission that someone has to fix, and the nodes should be up and refusing
// pods while that happens rather than absent.
const syncTimeout = 30 * time.Second

// Start begins the watch and waits, briefly, for its first sync.
//
// An error means the set is not known yet, not that it is empty, and the
// distinction matters: the caller must not read "no namespaces" as "serves
// nothing" and act on it.
func (n *Namespaces) Start(ctx context.Context) error {
	go n.informer.Run(ctx.Done())

	wait, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	if !cache.WaitForCacheSync(wait.Done(), n.informer.HasSynced) {
		return fmt.Errorf("the namespace watch did not sync within %s", syncTimeout)
	}
	log.G(ctx).WithField("namespaces", n.List()).Info("serving these namespaces")
	return nil
}
