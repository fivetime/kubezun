package ingress

import (
	"context"
	"fmt"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	discoveryv1informers "k8s.io/client-go/informers/discovery/v1"
	networkingv1informers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/fivetime/kubezun/pkg/service"
)

func isNotFoundErr(err error) bool { return apierrors.IsNotFound(err) }

// Controller keeps a tenant's L7 load balancers in step with their Ingresses.
type Controller struct {
	reconciler *Reconciler
	queue      workqueue.TypedRateLimitingInterface[string]
	synced     []cache.InformerSynced
}

// NewController wires a reconciler to the informers the process already runs.
func NewController(
	r *Reconciler,
	ingresses networkingv1informers.IngressInformer,
	slices discoveryv1informers.EndpointSliceInformer,
) (*Controller, error) {
	if r == nil {
		return nil, fmt.Errorf("a reconciler is required")
	}
	c := &Controller{
		reconciler: r,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	if _, err := ingresses.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueIngress(obj) },
		UpdateFunc: func(_, obj any) { c.enqueueIngress(obj) },
		// A deleted Ingress still has a load balancer, and the queue key is
		// all that is needed to find it: the name is derived, not stored.
		DeleteFunc: func(obj any) { c.enqueueIngress(obj) },
	}); err != nil {
		return nil, err
	}

	// An endpoint change is a member change. The slice informer is shared with
	// the Service controller; each fans out to its own consumers.
	if _, err := slices.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueSlice(obj) },
		UpdateFunc: func(_, obj any) { c.enqueueSlice(obj) },
		DeleteFunc: func(obj any) { c.enqueueSlice(obj) },
	}); err != nil {
		return nil, err
	}

	// No Secret watch, deliberately: this process caches no Secrets. A rotated
	// certificate is noticed by the periodic resync of the Ingress informer —
	// each resync re-reads the Secret one shot and the content hash decides
	// whether anything changes. Rotation lands within a resync period rather
	// than instantly, which certificates, renewed days ahead of expiry, can
	// well afford.

	c.synced = []cache.InformerSynced{
		ingresses.Informer().HasSynced, slices.Informer().HasSynced,
	}
	return c, nil
}

func (c *Controller) enqueueIngress(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	ing, ok := obj.(*networkingv1.Ingress)
	if !ok {
		return
	}
	c.queue.Add(ing.Namespace + "/" + ing.Name)
}

// enqueueSlice fans an EndpointSlice event out to the Ingresses in its
// namespace that reference the slice's Service as a backend.
func (c *Controller) enqueueSlice(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return
	}
	svcName := slice.Labels[discoveryv1.LabelServiceName]
	if svcName == "" {
		return
	}
	if !c.serves(slice.Namespace) {
		return
	}
	ings, err := c.reconciler.Ingresses.Ingresses(slice.Namespace).List(nil)
	if err != nil {
		return
	}
	for _, ing := range ings {
		if !c.reconciler.Ours(ing) {
			continue
		}
		_, backends := collectBackends(ing)
		for _, b := range backends {
			if b.Service == svcName {
				c.queue.Add(ing.Namespace + "/" + ing.Name)
				break
			}
		}
	}
}

// serves reports whether a namespace is one this process may build load
// balancers for. The informers span the cluster, so this is what keeps a
// tenant's credential from being spent on somebody else's Ingress — the same
// boundary, for the same reason, as the Service controller's (which once
// built nineteen load balancers for other people's Services without it).
func (c *Controller) serves(namespace string) bool {
	if c.reconciler.ServesNamespace == nil {
		return false
	}
	return c.reconciler.ServesNamespace(namespace)
}

// Run processes Ingresses until the context is cancelled. One worker, like the
// Service controller: Octavia serialises changes to a load balancer anyway.
func (c *Controller) Run(ctx context.Context) {
	defer c.queue.ShutDown()
	if !cache.WaitForCacheSync(ctx.Done(), c.synced...) {
		return
	}
	log.G(ctx).Info("ingress controller started")
	go func() {
		<-ctx.Done()
		c.queue.ShutDown()
	}()
	wait.UntilWithContext(ctx, c.processNext, time.Second)
}

func (c *Controller) processNext(ctx context.Context) {
	for {
		key, shutdown := c.queue.Get()
		if shutdown {
			return
		}
		c.reconcile(ctx, key)
		c.queue.Done(key)
	}
}

// recordFailure puts the reason on the Ingress, where kubectl describe finds
// it — until the reconcile succeeds the tenant sees no ADDRESS and the reason
// is otherwise only in this process's log, which they cannot read.
func (c *Controller) recordFailure(namespace, name string, cause error) {
	r := c.reconciler
	if r.Events == nil {
		return
	}
	ing, err := r.Ingresses.Ingresses(namespace).Get(name)
	if err != nil {
		return
	}
	r.Events.Eventf(ing, corev1.EventTypeWarning, "AddressNotReady",
		"the Ingress has no reachable address yet: %v", cause)
}

func (c *Controller) reconcile(ctx context.Context, key string) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.queue.Forget(key)
		return
	}
	if !c.serves(namespace) {
		c.queue.Forget(key)
		return
	}
	if err := c.reconciler.Reconcile(ctx, namespace, name); err != nil {
		log.G(ctx).WithError(err).WithField("ingress", key).
			Warn("reconciling the ingress load balancer failed; will retry")
		c.recordFailure(namespace, name, err)
		c.queue.AddRateLimited(key)
		return
	}
	c.queue.Forget(key)
}

// gcInterval matches the Service sweep's cadence and rationale.
const gcInterval = 5 * time.Minute

// RunGC removes ingress load balancers whose Ingress is gone, plus what hangs
// off them (floating IP, Barbican bundles). They accumulate whenever an
// Ingress is deleted while this process is not running.
func (c *Controller) RunGC(ctx context.Context) {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweep(ctx)
		}
	}
}

func (c *Controller) sweep(ctx context.Context) {
	r := c.reconciler

	// An empty served set means "does not know yet", never "serves nothing" —
	// sweeping on it would delete every live load balancer the tenant has.
	if r.Namespaces == nil || len(r.Namespaces()) == 0 {
		log.G(ctx).Warn("ingress sweep skipped: no namespaces are known yet")
		return
	}

	all, err := service.ListLoadBalancers(ctx, r.Octavia)
	if err != nil {
		log.G(ctx).WithError(err).Warn("ingress sweep skipped: could not list load balancers")
		return
	}
	for i := range all {
		lb := &all[i]
		namespace, name, ok := ParseLBName(r.Tenant, lb.Name)
		if !ok {
			continue // a Service's, another tenant's, or not ours at all
		}
		if !c.serves(namespace) {
			log.G(ctx).WithField("ingress", namespace+"/"+name).WithField("loadbalancer", lb.ID).
				Info("deleting an ingress load balancer for a namespace this process does not serve")
			if err := r.tearDown(ctx, lb.Name, lb); err != nil {
				log.G(ctx).WithError(err).WithField("loadbalancer", lb.ID).Warn("could not delete it")
			}
			continue
		}
		ing, err := r.Ingresses.Ingresses(namespace).Get(name)
		if err == nil && r.Ours(ing) && ing.DeletionTimestamp == nil {
			continue // alive and ours; the reconcile path owns it
		}
		if err != nil && !isNotFoundErr(err) {
			// The cache could not answer; deleting on that would take live
			// Ingresses off the air during an outage.
			log.G(ctx).WithError(err).WithField("ingress", namespace+"/"+name).
				Warn("ingress sweep skipped one: the lookup failed")
			continue
		}
		log.G(ctx).WithField("ingress", namespace+"/"+name).
			WithField("loadbalancer", lb.ID).Info("deleting orphaned ingress load balancer")
		if err := r.tearDown(ctx, lb.Name, lb); err != nil {
			log.G(ctx).WithError(err).WithField("loadbalancer", lb.ID).
				Warn("could not delete an orphaned ingress load balancer")
		}
	}
}
