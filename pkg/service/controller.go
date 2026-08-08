package service

import (
	"context"
	"fmt"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	discoveryv1informers "k8s.io/client-go/informers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller keeps a tenant's load balancers in step with their Services.
type Controller struct {
	reconciler *Reconciler
	queue      workqueue.TypedRateLimitingInterface[string]
	synced     []cache.InformerSynced
}

// NewController wires a reconciler to the informers a node already runs.
func NewController(
	r *Reconciler,
	services corev1informers.ServiceInformer,
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

	if _, err := services.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueService(obj) },
		UpdateFunc: func(_, obj any) { c.enqueueService(obj) },
		// A deleted Service still has a load balancer, and the queue key is all
		// that is needed to find it: the name it was given is derived from the
		// namespace and name, not stored on the object.
		DeleteFunc: func(obj any) { c.enqueueService(obj) },
	}); err != nil {
		return nil, err
	}

	// An endpoint change is a member change, which is how a pod becoming ready
	// or going away reaches the load balancer.
	if _, err := slices.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueSlice(obj) },
		UpdateFunc: func(_, obj any) { c.enqueueSlice(obj) },
		DeleteFunc: func(obj any) { c.enqueueSlice(obj) },
	}); err != nil {
		return nil, err
	}

	c.synced = []cache.InformerSynced{
		services.Informer().HasSynced, slices.Informer().HasSynced,
	}
	return c, nil
}

func (c *Controller) enqueueService(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	svc, ok := obj.(*corev1.Service)
	if !ok {
		return
	}
	c.queue.Add(svc.Namespace + "/" + svc.Name)
}

func (c *Controller) enqueueSlice(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	slice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return
	}
	name := slice.Labels[discoveryv1.LabelServiceName]
	if name == "" {
		return
	}
	c.queue.Add(slice.Namespace + "/" + name)
}

// Run processes Services until the context is cancelled.
//
// One worker on purpose: Octavia serialises changes to a load balancer anyway,
// refusing anything while one is still provisioning, so concurrent workers on
// the same tenant would spend their time waiting on each other. A tenant's
// Services are few, and each reconcile is bounded.
func (c *Controller) Run(ctx context.Context) {
	defer c.queue.ShutDown()

	if !cache.WaitForCacheSync(ctx.Done(), c.synced...) {
		return
	}
	log.G(ctx).Info("service controller started")

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

// recordFailure puts the reason on the Service, where kubectl describe finds it.
func (c *Controller) recordFailure(namespace, name string, cause error) {
	r := c.reconciler
	if r.Events == nil {
		return
	}
	svc, err := r.Services.Services(namespace).Get(name)
	if err != nil {
		return
	}
	r.Events.Eventf(svc, corev1.EventTypeWarning, "AddressNotReady",
		"the Service has no reachable address yet: %v", cause)
}

func (c *Controller) reconcile(ctx context.Context, key string) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		c.queue.Forget(key)
		return
	}

	if err := c.reconciler.Reconcile(ctx, namespace, name); err != nil {
		// Retried with backoff rather than dropped: a load balancer that is
		// still provisioning, or an OpenStack call that failed once, resolves
		// itself, and giving up would leave the Service pointing at nothing
		// with no further attempt.
		log.G(ctx).WithError(err).WithField("service", key).
			Warn("reconciling the load balancer failed; will retry")
		// Recorded on the Service as well: until this succeeds the tenant is
		// shown an address that does not work, and the reason is otherwise
		// only in this process's log, which they cannot read.
		c.recordFailure(namespace, name, err)
		c.queue.AddRateLimited(key)
		return
	}
	c.queue.Forget(key)
}
