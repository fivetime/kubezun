package vknode

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func ns(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// Before the watch has synced nothing is served. This is the authorization
// boundary, and answering "yes" while the set is unknown would run another
// tenant's pod in this tenant's OpenStack project — which no later correction
// undoes, because the capsule has already been created and charged for.
func TestServesNothingBeforeSync(t *testing.T) {
	n := NewNamespaces(fake.NewSimpleClientset(), "kubezoo.io/tenant=111111")
	if n.Serves("111111-default") {
		t.Fatal("served a namespace before the watch had synced")
	}
}

func TestServesTracksTheSet(t *testing.T) {
	n := NewNamespaces(fake.NewSimpleClientset(), "kubezoo.io/tenant=111111")

	n.add(ns("111111-default"))
	n.add(ns("111111-kube-system"))
	if !n.Serves("111111-default") || !n.Serves("111111-kube-system") {
		t.Fatal("a namespace that arrived is not served")
	}
	if n.Serves("222222-default") {
		t.Fatal("served a namespace that never arrived")
	}

	n.remove(ns("111111-kube-system"))
	if n.Serves("111111-kube-system") {
		t.Fatal("still serving a namespace that went away")
	}
	if !n.Serves("111111-default") {
		t.Fatal("removing one namespace dropped another")
	}
}

// A namespace created after this process started is the case a fixed list gets
// wrong: its pods would stay Pending forever with nothing saying why.
func TestObserversHearAboutLateNamespaces(t *testing.T) {
	n := NewNamespaces(fake.NewSimpleClientset(), "kubezoo.io/tenant=111111")
	n.add(ns("111111-default"))

	var added []string
	n.OnChange(func(a, _ []string) { added = append(added, a...) })
	// Registering reports what is already there, so an observer added after
	// the watch started does not miss the namespaces that exist.
	if len(added) != 1 || added[0] != "111111-default" {
		t.Fatalf("a late observer was not told about existing namespaces: %v", added)
	}

	n.add(ns("111111-staging"))
	if len(added) != 2 || added[1] != "111111-staging" {
		t.Fatalf("an observer was not told about a new namespace: %v", added)
	}
}

// The same namespace arriving twice — a resync, or a relist after a watch
// breaks — must not be reported as new, or every observer rebuilds its state
// on a schedule.
func TestDuplicateAddIsNotAChange(t *testing.T) {
	n := NewNamespaces(fake.NewSimpleClientset(), "kubezoo.io/tenant=111111")

	var changes int
	n.OnChange(func(_, _ []string) { changes++ })

	n.add(ns("111111-default"))
	n.add(ns("111111-default"))
	if changes != 1 {
		t.Fatalf("a repeated add was reported %d times", changes)
	}
}
