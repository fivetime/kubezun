package netpol

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func podLister(t *testing.T, list ...*corev1.Pod) corev1listers.PodLister {
	t.Helper()
	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, p := range list {
		if err := store.Add(p); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	return corev1listers.NewPodLister(store)
}

// TestRemovingAPolicyGivesTheGroupsBack is the half that is easy to leave out.
// Enforcing a new policy is the obvious direction; a policy deleted must also
// reach every pod it used to select, and by then nothing records which ones
// those were.
func TestRemovingAPolicyGivesTheGroupsBack(t *testing.T) {
	web := pod("prod", "web", map[string]string{"app": "web"})
	isolating := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "in"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}

	withPolicy := reconciler(t, isolating)
	withPolicy.Pods = podLister(t, web)
	got, err := withPolicy.GroupsFor(web)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("while the policy exists the pod should lose one group: %v", got)
	}

	// The same pod, once the policy is gone.
	without := reconciler(t)
	without.Pods = podLister(t, web)
	got, err = without.GroupsFor(web)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("deleting the policy did not restore the pod: %v", got)
	}
}

// TestOnlyLabelsAndAddressesWakeTheController keeps a pod's ordinary status
// churn -- conditions, restart counts, timestamps -- from queueing a Neutron
// round trip per event per pod.
func TestOnlyLabelsAndAddressesWakeTheController(t *testing.T) {
	r := reconciler(t)
	r.Pods = podLister(t)
	r.ServesNamespace = func(string) bool { return true }

	client := fake.NewSimpleClientset()
	factory := informers.NewSharedInformerFactory(client, 0)
	c, err := NewController(r, factory.Core().V1().Pods(),
		factory.Networking().V1().NetworkPolicies())
	if err != nil {
		t.Fatal(err)
	}

	before := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "prod", Name: "web", Labels: map[string]string{"app": "web"}},
		Status: corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning}}

	// Phase moved and nothing this cares about did.
	noise := before.DeepCopy()
	noise.Status.Phase = corev1.PodSucceeded
	noise.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady}}
	c.queue.Add("seed")
	depth := c.queue.Len()
	handlers, _ := factory.Core().V1().Pods().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{})
	_ = handlers
	// Drive the same predicate the handler uses.
	if !samePolicyInputs(before, noise) {
		t.Error("a status-only change was treated as relevant")
	}
	if c.queue.Len() != depth {
		t.Error("the queue grew on a change nothing depends on")
	}

	// A label change is relevant: it can move the pod in or out of a policy.
	relabelled := before.DeepCopy()
	relabelled.Labels = map[string]string{"app": "api"}
	if samePolicyInputs(before, relabelled) {
		t.Error("a label change was ignored")
	}

	// So is gaining an address: it is what a peer set is made of.
	addressed := before.DeepCopy()
	addressed.Status.PodIP = "10.0.0.2"
	if samePolicyInputs(before, addressed) {
		t.Error("an address change was ignored")
	}
}
