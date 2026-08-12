package netpol

import (
	"testing"

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
	if c.policies.Len() == 0 {
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
	if c.policies.Len() == 0 {
		t.Error("a deleted peer pod queued nothing; its address would stay in " +
			"the set until something else happened to touch the policy")
	}
	for c.policies.Len() > 0 {
		key, _ := c.policies.Get()
		c.policies.Done(key)
	}

	// A pod in a namespace this process does not serve is not its business.
	c.enqueuePoliciesNear(pod("other", "web", map[string]string{"app": "web"}))
	if c.policies.Len() != 0 {
		t.Error("queued work for a namespace this process does not serve")
	}
}
