package netpol

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	networkingv1listers "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
)

func policyLister(t *testing.T, list ...*networkingv1.NetworkPolicy) networkingv1listers.NetworkPolicyLister {
	t.Helper()
	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, p := range list {
		if err := store.Add(p); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	return networkingv1listers.NewNetworkPolicyLister(store)
}

func reconciler(t *testing.T, list ...*networkingv1.NetworkPolicy) *Reconciler {
	r := &Reconciler{Policies: policyLister(t, list...)}
	r.baseline.ingress, r.baseline.egress = "sg-in", "sg-eg"
	return r
}

func pod(ns, name string, kv map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: name, Labels: kv}}
}

// TestAnUnselectedPodKeepsBothGroups is the case that must not regress: a
// tenant with no policies at all must find every pod able to talk, because
// Neutron's baseline is deny and Kubernetes' is allow.
func TestAnUnselectedPodKeepsBothGroups(t *testing.T) {
	r := reconciler(t)
	got, err := r.GroupsFor(pod("prod", "web", map[string]string{"app": "web"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("a pod no policy selects lost a group: %v", got)
	}
}

// TestIsolationTakesOnlyTheDirectionAsked is why there are two groups. A
// Neutron security group has no direction, so a single combined group would
// take egress away from a pod whose policy named only Ingress.
func TestIsolationTakesOnlyTheDirectionAsked(t *testing.T) {
	r := reconciler(t, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "in"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	})

	got, err := r.GroupsFor(pod("prod", "web", map[string]string{"app": "web"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "sg-eg" {
		t.Errorf("an ingress policy should leave egress alone: %v", got)
	}

	// A pod the policy does not select is untouched.
	got, _ = r.GroupsFor(pod("prod", "db", map[string]string{"app": "db"}))
	if len(got) != 2 {
		t.Errorf("an unselected pod was isolated: %v", got)
	}

	// ⚠️ Same labels, another namespace. A tenant's namespaces share one
	// Neutron network, so a policy leaking across them would be invisible in
	// the substrate and wrong in exactly the case namespaces are made for.
	got, _ = r.GroupsFor(pod("staging", "web", map[string]string{"app": "web"}))
	if len(got) != 2 {
		t.Errorf("a policy reached into another namespace: %v", got)
	}
}

// TestBothDirectionsLeaveNothing checks the deny-all case reaches the state it
// means, rather than falling back to something permissive.
func TestBothDirectionsLeaveNothing(t *testing.T) {
	r := reconciler(t, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "all"},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	})
	got, err := r.GroupsFor(pod("prod", "web", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("deny-all in both directions still carried groups: %v", got)
	}
}

// TestBaselineMustBeResolvedFirst keeps a startup ordering mistake from
// quietly producing a pod with no groups, which reads as deny-all.
func TestBaselineMustBeResolvedFirst(t *testing.T) {
	r := &Reconciler{Policies: policyLister(t)}
	if _, err := r.GroupsFor(pod("prod", "web", nil)); err == nil {
		t.Error("groups were computed before the baseline existed")
	}
}
