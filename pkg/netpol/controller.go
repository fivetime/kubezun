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
	// pods carries "which groups should this port hold".
	queue workqueue.TypedRateLimitingInterface[string]
	// policies carries "what is in the address groups this policy refers to".
	//
	// ⚠️ A second queue rather than more work on the first. Peer membership is
	// a property of the policy; the pods in it are ones the policy does NOT
	// select, so reconciling a pod cannot maintain the sets it belongs to --
	// which is how a new replica of a peer was never added and a deleted one
	// never removed.
	policies workqueue.TypedRateLimitingInterface[string]
	synced   []cache.InformerSynced
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
		policies: workqueue.NewTypedRateLimitingQueue(
			workqueue.DefaultTypedControllerRateLimiter[string]()),
	}

	if _, err := pods.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { c.enqueuePod(obj); c.enqueuePoliciesNear(obj) },
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
			c.enqueuePoliciesNear(cur)
		},
		// ⚠️ A deleted pod must reach the peer sets it was in. Without this its
		// address stays allowed for ever -- and Neutron reuses addresses, so
		// the next capsule to be given it inherits the access, whatever its
		// labels. The one direction in this package that fails open.
		DeleteFunc: func(obj any) { c.enqueuePoliciesNear(obj) },
	}); err != nil {
		return nil, err
	}

	if _, err := policies.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { c.enqueueNamespaceOf(obj); c.enqueuePolicy(obj) },
		UpdateFunc: func(_, cur any) {
			c.enqueueNamespaceOf(cur)
			c.enqueuePolicy(cur)
		},
		// A deleted policy leaves its groups to the sweep; there is nothing
		// left whose peers could need syncing.
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

// enqueuePoliciesNear queues every policy that could have this pod as a peer.
//
// Every policy in every namespace this process serves -- not only the pod's
// own. ⚠️ A namespaceSelector peer reaches across namespaces, so a policy in
// one namespace routinely names pods in another; queueing only the pod's
// namespace leaves those policies waiting for an event that never comes. That
// is the case DESIGN §7.7 opens with: a tenant's namespaces share one project,
// and separating prod from staging is the usual reason to make a second one.
//
// Every policy rather than the ones that actually match: a peer is by
// definition a pod the policy does not select, so nothing on the pod says
// which policies care about it. Recomputing is cheaper than the reverse index
// that would answer it, and the sets are what has to be right.
func (c *Controller) enqueuePoliciesNear(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok || !c.reconciler.ServesNamespace(pod.Namespace) {
		return
	}
	list, err := c.reconciler.Policies.List(labels.Everything())
	if err != nil {
		return
	}
	for _, p := range list {
		if c.reconciler.ServesNamespace(p.Namespace) {
			c.enqueuePolicyKey(p.Namespace + "/" + p.Name)
		}
	}
}

// PeerCoalesce is how long a policy waits before its peer sets are recomputed.
//
// ⚠️ Every pod event queues every policy, so without a wait the work follows
// the rate pods come and go rather than the rate the sets actually change --
// and on a serverless line those are very different numbers. A delaying queue
// collapses repeats of a key that is still waiting, so a burst of pod churn
// becomes one synchronisation per policy. A second is far below anything a
// person notices and far above the length of a rollout's thundering herd.
const PeerCoalesce = time.Second

func (c *Controller) enqueuePolicyKey(key string) {
	c.policies.AddAfter(key, PeerCoalesce)
}

func (c *Controller) enqueuePolicy(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	p, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok || !c.reconciler.ServesNamespace(p.Namespace) {
		return
	}
	c.enqueuePolicyKey(p.Namespace + "/" + p.Name)
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
	defer c.policies.ShutDown()
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
		go wait.UntilWithContext(ctx, c.workPolicies, time.Second)
	}
	go wait.UntilWithContext(ctx, c.sweep, SweepInterval)
	// ⚠️ And a full recomputation on a timer, so a peer set that drifted for
	// any reason -- a missed event, a failed write retried past its limit --
	// comes back on its own rather than staying wrong until somebody edits
	// the policy. A set that can only be corrected by hand is a set that
	// stays wrong.
	go wait.UntilWithContext(ctx, c.resyncPolicies, PeerResyncInterval)
	<-ctx.Done()
	return nil
}

// SweepInterval is how often unused groups are collected.
//
// ⚠️ Slow on purpose. Removing a security group object makes ovn-northd
// rebuild every port group in the cloud, so the cost is paid by every other
// tenant, by nova and by Octavia. A tenant editing policies through an
// afternoon should cost that once, not once per edit -- and nothing is
// harmed by a group that outlives its policy by half an hour.
const SweepInterval = 30 * time.Minute

// PeerResyncInterval is how often every policy's peer sets are recomputed
// whether anything looked like it changed or not.
//
// Cheaper than it sounds and worth it anyway: the work is one read per address
// group and a write only when the contents differ, and what it buys is that no
// peer set can stay wrong indefinitely. Membership drifting quietly is the
// failure this whole path had.
const PeerResyncInterval = 10 * time.Minute

func (c *Controller) workPolicies(ctx context.Context) {
	for {
		key, quit := c.policies.Get()
		if quit {
			return
		}
		if err := c.syncPolicy(ctx, key); err != nil {
			log.G(ctx).WithError(err).WithField("policy", key).
				Warn("could not update this policy's peer sets; will retry")
			c.policies.AddRateLimited(key)
		} else {
			c.policies.Forget(key)
		}
		c.policies.Done(key)
	}
}

func (c *Controller) syncPolicy(ctx context.Context, key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	if !c.reconciler.ServesNamespace(namespace) {
		return nil
	}
	p, err := c.reconciler.Policies.NetworkPolicies(namespace).Get(name)
	if err != nil {
		// Gone. Its groups are the sweep's business.
		return nil
	}
	return c.reconciler.SyncPolicyPeers(ctx, p)
}

func (c *Controller) resyncPolicies(ctx context.Context) {
	list, err := c.reconciler.Policies.List(labels.Everything())
	if err != nil {
		return
	}
	for _, p := range list {
		if c.reconciler.ServesNamespace(p.Namespace) {
			c.enqueuePolicyKey(p.Namespace + "/" + p.Name)
		}
	}
}

func (c *Controller) sweep(ctx context.Context) {
	live := map[string]bool{}
	policies, err := c.reconciler.Policies.List(labels.Everything())
	if err != nil {
		log.G(ctx).WithError(err).Warn("skipping the group sweep; the policy list is unavailable")
		return
	}
	for _, p := range policies {
		live[p.Namespace+"/"+p.Name] = true
	}
	// ⚠️ An empty live set with a populated cache is a tenant with no
	// policies, which is ordinary. An empty one because the cache never
	// synced would delete every group the tenant has, so the sweep runs only
	// after the caches are up -- which Run has already waited for.
	removed, err := c.reconciler.Neutron.Sweep(ctx, live, c.reconciler.ServesNamespace)
	if err != nil {
		log.G(ctx).WithError(err).Warn("the group sweep did not finish; it will run again")
	}
	if removed > 0 {
		log.G(ctx).WithField("removed", removed).
			Info("collected security and address groups no policy needs")
	}
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

	want, err := r.GroupsFor(ctx, pod)
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
