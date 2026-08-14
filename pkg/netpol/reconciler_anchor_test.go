package netpol

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestFullyIsolatedPodNeverGetsAnEmptyGroupList pins the deny-all anchor.
//
// ⚠️ This is a structural guard, not bookkeeping, and it has been proposed for
// deletion once already (2026-08-14 review: "delete the anchor, save a third of
// the port-group rebuilds"). The reason it must stay is one layer up:
// zun.TemplateOptions.SecurityGroups is marshaled with omitempty, so an empty
// list VANISHES from the template, the field is absent, and Zun gives the port
// the project's default group — a fully isolated pod comes up reachable by
// everything, silently. The anchor makes the empty list unreachable, which
// makes every layer's treatment of emptiness irrelevant. Delete it only if
// every marshaling and schema layer between here and Neutron provably
// preserves an explicit empty list — and this test is where that proof would
// have to arrive.
func TestFullyIsolatedPodNeverGetsAnEmptyGroupList(t *testing.T) {
	both := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "lockdown"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{}, // every pod in the namespace
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
	r := reconciler(t, both)
	r.Pods = podLister(t)
	r.ServesNamespace = func(ns string) bool { return ns == "prod" }
	r.baseline.ingress, r.baseline.egress, r.baseline.denyAll = "allow-in", "allow-eg", "anchor"

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "prod", Name: "locked", Labels: map[string]string{"app": "x"}}}
	groups, err := r.GroupsFor(t.Context(), pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("a fully isolated pod got an empty group list — through " +
			"omitempty that is no securityGroups field at all, and the port " +
			"comes up on the project default: wide open")
	}
	found := false
	for _, g := range groups {
		if g == "anchor" {
			found = true
		}
		if g == "allow-in" || g == "allow-eg" {
			t.Fatalf("an isolated direction got its baseline allow group: %v", groups)
		}
	}
	if !found {
		t.Fatalf("the deny-all anchor is missing from %v", groups)
	}
}
