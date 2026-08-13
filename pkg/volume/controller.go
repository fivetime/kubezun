package volume

import (
	"context"
	"fmt"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller keeps a tenant's claims and the storage behind them in step.
type Controller struct {
	reconciler *Reconciler

	// ReconcilerFor and EachReconciler are the multi-tenant seams, same
	// contract as the Service controller's: per-namespace resolution for
	// claims, a walk for the released-volume sweep. Nil means the one fixed
	// reconciler serves everything.
	ReconcilerFor  func(ctx context.Context, namespace string) (*Reconciler, error)
	EachReconciler func(ctx context.Context, fn func(*Reconciler) error) error
	queue          workqueue.TypedRateLimitingInterface[string]
	synced         []cache.InformerSynced
}

// NewController wires the reconciler to the informers the process already runs.
func NewController(r *Reconciler,
	claims corev1informers.PersistentVolumeClaimInformer,
	volumes corev1informers.PersistentVolumeInformer) (*Controller, error) {
	if r == nil {
		return nil, fmt.Errorf("a reconciler is required")
	}
	c := &Controller{
		reconciler: r,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
	}
	if _, err := claims.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueue(obj) },
		UpdateFunc: func(_, obj any) { c.enqueue(obj) },
		// A deleted claim leaves a Released volume behind; the sweep owns
		// that, so deletion enqueues nothing.
	}); err != nil {
		return nil, err
	}
	c.synced = []cache.InformerSynced{
		claims.Informer().HasSynced, volumes.Informer().HasSynced,
	}
	return c, nil
}

func (c *Controller) enqueue(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	claim, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return
	}
	c.queue.Add(claim.Namespace + "/" + claim.Name)
}

// Run processes claims until the context is cancelled.
func (c *Controller) Run(ctx context.Context) {
	defer c.queue.ShutDown()
	if !cache.WaitForCacheSync(ctx.Done(), c.synced...) {
		return
	}
	log.G(ctx).Info("persistent volume controller started")
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
		namespace, name, err := cache.SplitMetaNamespaceKey(key)
		if err == nil {
			var r *Reconciler
			if r, err = c.reconcilerOf(ctx, namespace); err == nil {
				err = r.Reconcile(ctx, namespace, name)
			}
			if err != nil {
				log.G(ctx).WithError(err).WithField("claim", key).
					Warn("provisioning the claim failed; will retry")
				c.queue.AddRateLimited(key)
				c.queue.Done(key)
				continue
			}
		}
		c.queue.Forget(key)
		c.queue.Done(key)
	}
}

// gcInterval matches the other sweeps' cadence and rationale.
const gcInterval = 5 * time.Minute

// RunGC removes the storage behind volumes whose claim is gone. Nothing else
// in this cluster will: there is no CSI controller, which is why this package
// exists.
func (c *Controller) RunGC(ctx context.Context) {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = c.eachReconciler(ctx, func(r *Reconciler) error {
				r.SweepReleased(ctx)
				return nil
			})
		}
	}
}

func (c *Controller) reconcilerOf(ctx context.Context, namespace string) (*Reconciler, error) {
	if c.ReconcilerFor == nil {
		return c.reconciler, nil
	}
	return c.ReconcilerFor(ctx, namespace)
}

func (c *Controller) eachReconciler(ctx context.Context, fn func(*Reconciler) error) error {
	if c.EachReconciler == nil {
		return fn(c.reconciler)
	}
	return c.EachReconciler(ctx, fn)
}
