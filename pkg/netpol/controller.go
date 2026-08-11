package netpol

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	networkingv1informers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller keeps the groups on a running pod's port in step with the
// policies that select it.
//
// ⚠️ Without this, a NetworkPolicy would apply only to pods created after it.
// That is worse than not enforcing it at all: the tenant sees the policy
// listed, believes it is in force, and half the pods it names ignore it. A
// policy that does nothing is a bug someone notices.
type Controller struct {
	reconciler *Reconciler
	queue      workqueue.TypedRateLimitingInterface[string]
	synced     []cache.InformerSynced
}

// NewController wires the reconciler to the informers this process runs.
//
// A policy event enqueues every pod in its namespace rather than the pods it
// selects: a policy that has just stopped selecting a pod has to reach that pod
// too, and by the time the event arrives there is nothing left to say which
// pods those were.
func NewController(r *Reconciler, pods corev1informers.PodInformer,
	policies networkingv1informers.NetworkPolicyInformer) (*Controller, error) {
	if r == nil {
		return nil, fmt.Errorf("a reconciler is required")
	}
	c := &Controller{
		reconciler: r,
		queue: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	if _, err := pods.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { c.enqueuePod(obj) },
		UpdateFunc: func(old, cur any) {
			// Labels decide which policies select a pod, and an address is
			// what a peer set is made of. Everything else about a pod changes
			// constantly and changes nothing here.
			a, aok := old.(*corev1.Pod)
			b, bok := cur.(*corev1.Pod)
			if aok && bok && samePolicyInputs(a, b) {
				return
			}
			c.enqueuePod(cur)
		},
	}); err != nil {
		return nil, err
	}

	if _, err := policies.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.enqueueNamespaceOf(obj) },
		UpdateFunc: func(_, cur any) { c.enqueueNamespaceOf(cur) },
		DeleteFunc: func(obj any) { c.enqueueNamespaceOf(obj) },
	}); err != nil {
		return nil, err
	}

	c.synced = []cache.InformerSynced{
		pods.Informer().HasSynced, policies.Informer().HasSynced,
	}
	return c, nil
}

func (c *Controller) enqueuePod(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok || !c.reconciler.ServesNamespace(pod.Namespace) {
		return
	}
	c.queue.Add(pod.Namespace + "/" + pod.Name)
}

func (c *Controller) enqueueNamespaceOf(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	p, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok || !c.reconciler.ServesNamespace(p.Namespace) {
		return
	}
	pods, err := c.reconciler.Pods.Pods(p.Namespace).List(labels.Everything())
	if err != nil {
		return
	}
	for _, pod := range pods {
		c.queue.Add(pod.Namespace + "/" + pod.Name)
	}
}

// Run reconciles pods until the context is cancelled.
func (c *Controller) Run(ctx context.Context, workers int) error {
	defer c.queue.ShutDown()
	if !cache.WaitForCacheSync(ctx.Done(), c.synced...) {
		return fmt.Errorf("the caches this needs never synced")
	}
	// The baseline has to exist before any pod is judged: computing groups
	// without it yields nothing, and nothing means deny-all.
	if err := c.reconciler.EnsureBaseline(ctx); err != nil {
		return fmt.Errorf("preparing the baseline security groups: %w", err)
	}
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.work, time.Second)
	}
	<-ctx.Done()
	return nil
}

func (c *Controller) work(ctx context.Context) {
	for {
		key, quit := c.queue.Get()
		if quit {
			return
		}
		if err := c.reconciler.ReconcilePod(ctx, key); err != nil {
			log.G(ctx).WithError(err).WithField("pod", key).
				Warn("could not align this pod's security groups; will retry")
			c.queue.AddRateLimited(key)
		} else {
			c.queue.Forget(key)
		}
		c.queue.Done(key)
	}
}

// ReconcilePod brings one pod's port in line with the policies selecting it.
func (r *Reconciler) ReconcilePod(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	if !r.ServesNamespace(namespace) {
		return nil
	}
	pod, err := r.Pods.Pods(namespace).Get(name)
	if err != nil {
		// Gone. Its port went with its capsule; there is nothing to align.
		return nil
	}
	if pod.DeletionTimestamp != nil {
		return nil
	}
	portID := ""
	if r.PortOf != nil {
		portID = r.PortOf(pod)
	}
	if portID == "" {
		// Not placed yet. The groups it is created with come from the same
		// function, so there is nothing to correct.
		return nil
	}

	want, err := r.GroupsFor(pod)
	if err != nil {
		return err
	}
	current, err := ports.Get(ctx, r.Neutron.Client, portID).Extract()
	if err != nil {
		return fmt.Errorf("reading port %s: %w", portID, err)
	}
	have := append([]string(nil), current.SecurityGroups...)
	sort.Strings(have)
	if equalStrings(have, want) {
		return nil
	}

	// ⚠️ Written as the whole list, not as a difference. A port's groups are a
	// set the API replaces wholesale, and computing a delta against a read
	// that may already be stale is how two reconcilers talking to one port
	// leave it holding the union of two different answers.
	log.G(ctx).WithField("pod", key).WithField("port", portID).
		WithField("from", have).WithField("to", want).
		Info("aligning security groups with the policies that select this pod")
	if _, err := ports.Update(ctx, r.Neutron.Client, portID, ports.UpdateOpts{
		SecurityGroups: &want,
	}).Extract(); err != nil {
		return fmt.Errorf("updating the groups on port %s: %w", portID, err)
	}
	return nil
}

// samePolicyInputs reports whether nothing this controller depends on changed.
// Labels decide which policies select a pod; an address is what a peer set is
// made of. A pod's status churns constantly and means nothing here.
func samePolicyInputs(a, b *corev1.Pod) bool {
	return reflect.DeepEqual(a.Labels, b.Labels) &&
		a.Status.PodIP == b.Status.PodIP
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
