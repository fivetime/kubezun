package netpol

import (
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func peerPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "db-from-web"},
		Spec: networkingv1.NetworkPolicySpec{
			// ⚠️ Subject and peer are different pods. That is the ordinary
			// case, and it is what the bug below turned on.
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "web"}},
				}},
			}},
		},
	}
}

// TestAPeerPodQueuesThePoliciesItMightBeIn is the fix for a set that could only
// drift. Peer membership belongs to the policy, but it used to be computed only
// while reconciling one of the policy's SUBJECT pods -- and a peer is by
// definition a pod the policy does not select. So a new replica of `web` was
// never added to the set that lets it reach `db`, and the symptom was two
// replicas out of three working, permanently.
func TestAPeerPodQueuesThePoliciesItMightBeIn(t *testing.T) {
	r := reconciler(t, peerPolicy())
	r.Pods = podLister(t)
	r.ServesNamespace = func(ns string) bool { return ns == "prod" }

	factory := informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0)
	c, err := NewController(r, factory.Core().V1().Pods(),
		factory.Networking().V1().NetworkPolicies())
	if err != nil {
		t.Fatal(err)
	}

	// A pod the policy does not select, and never will.
	web := pod("prod", "web-3", map[string]string{"app": "web"})
	c.enqueuePoliciesNear(web)
	if !waitForWork(c) {
		t.Fatal("a peer pod queued no policy, so nothing will ever add it to " +
			"the set that admits it")
	}

	// Drain it: a work queue holds a key added while the same key is being
	// processed, so the next assertion needs a clean one.
	for c.policies.Len() > 0 {
		key, _ := c.policies.Get()
		c.policies.Done(key)
	}

	// ⚠️ And on deletion too. Without it the address stays allowed for ever,
	// and Neutron hands reused addresses to whatever comes next -- which then
	// inherits access it was never granted.
	c.enqueuePoliciesNear(cache.DeletedFinalStateUnknown{Key: "prod/web-3", Obj: web})
	if !waitForWork(c) {
		t.Error("a deleted peer pod queued nothing; its address would stay in " +
			"the set until something else happened to touch the policy")
	}
	for c.policies.Len() > 0 {
		key, _ := c.policies.Get()
		c.policies.Done(key)
	}

	// A pod in a namespace this process does not serve is not its business.
	c.enqueuePoliciesNear(pod("other", "web", map[string]string{"app": "web"}))
	time.Sleep(PeerCoalesce + 200*time.Millisecond)
	if c.policies.Len() != 0 {
		t.Error("queued work for a namespace this process does not serve")
	}
}

// TestACrossNamespacePeerIsQueuedToo covers the half the first fix left out.
// A namespaceSelector peer reaches into other namespaces, so a policy in
// staging can name pods in prod -- and queueing only the changed pod's own
// namespace leaves that policy waiting for an event that never arrives.
//
// ⚠️ The delete direction is the one that matters: until the ten-minute resync
// catches it, a departed pod's address stays allowed, and Neutron reuses
// addresses inside that window.
func TestACrossNamespacePeerIsQueuedToo(t *testing.T) {
	// The policy lives in staging; its peers are pods in prod.
	crossNS := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "staging", Name: "from-prod"},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"env": "prod"}},
				}},
			}},
		},
	}
	r := reconciler(t, crossNS)
	r.Pods = podLister(t)
	r.ServesNamespace = func(ns string) bool { return ns == "prod" || ns == "staging" }

	factory := informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0)
	c, err := NewController(r, factory.Core().V1().Pods(),
		factory.Networking().V1().NetworkPolicies())
	if err != nil {
		t.Fatal(err)
	}

	c.enqueuePoliciesNear(pod("prod", "web", map[string]string{"app": "web"}))
	// The queue delays on purpose, so wait for the coalescing window rather
	// than reading an empty queue and calling it a failure.
	if !waitForWork(c) {
		t.Fatal("a pod change in prod queued no policy in staging, so a policy " +
			"naming it as a peer would never hear about it")
	}
	key, _ := c.policies.Get()
	if key != "staging/from-prod" {
		t.Errorf("queued %q", key)
	}
	c.policies.Done(key)
}

// TestABurstOfPodEventsCollapses keeps Neutron traffic following the rate the
// sets change rather than the rate pods come and go -- which on a serverless
// line are very different numbers.
func TestABurstOfPodEventsCollapses(t *testing.T) {
	r := reconciler(t, peerPolicy())
	r.Pods = podLister(t)
	r.ServesNamespace = func(string) bool { return true }
	factory := informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0)
	c, err := NewController(r, factory.Core().V1().Pods(),
		factory.Networking().V1().NetworkPolicies())
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 50; i++ {
		c.enqueuePoliciesNear(pod("prod", "web", map[string]string{"app": "web"}))
	}
	waitForWork(c)
	if got := c.policies.Len(); got != 1 {
		t.Errorf("fifty pod events produced %d units of work, want 1", got)
	}
}

// waitForWork waits out the coalescing window. The queue delays on purpose, so
// reading its length immediately after enqueueing measures the delay rather
// than the behaviour.
func waitForWork(c *Controller) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.policies.Len() > 0 {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
