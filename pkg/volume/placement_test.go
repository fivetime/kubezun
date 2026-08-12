package volume

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	storagev1listers "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
)

func classes(t *testing.T, list ...*storagev1.StorageClass) storagev1listers.StorageClassLister {
	t.Helper()
	store := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, c := range list {
		if err := store.Add(c); err != nil {
			t.Fatalf("seeding the class lister: %v", err)
		}
	}
	return storagev1listers.NewStorageClassLister(store)
}

func class(name string, mode *storagev1.VolumeBindingMode) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: name},
		Provisioner:       BlockDriver,
		VolumeBindingMode: mode,
	}
}

func claimOf(className, selectedNode string) *corev1.PersistentVolumeClaim {
	c := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "n"},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &className},
	}
	if selectedNode != "" {
		c.Annotations = map[string]string{selectedNodeAnnotation: selectedNode}
	}
	return c
}

// TestWaitsOnlyWhenTheClassSaysTo keeps deferred binding from becoming a
// deadlock. A class that never defers must provision straight away; one that
// does must not provision until the scheduler has said where.
func TestWaitsOnlyWhenTheClassSaysTo(t *testing.T) {
	wffc := storagev1.VolumeBindingWaitForFirstConsumer
	immediate := storagev1.VolumeBindingImmediate

	r := &Reconciler{Classes: classes(t,
		class("deferred", &wffc),
		class("now", &immediate),
		class("unset", nil))}

	for _, tc := range []struct {
		name  string
		claim *corev1.PersistentVolumeClaim
		wait  bool
	}{
		{"deferred and unplaced", claimOf("deferred", ""), true},
		{"deferred and placed", claimOf("deferred", "tenant-node-az1"), false},
		{"immediate", claimOf("now", ""), false},
		// An unset binding mode means Immediate. Reading nil as "wait" would
		// hang every claim of every class written before this existed.
		{"binding mode unset", claimOf("unset", ""), false},
		{"class missing entirely", claimOf("gone", ""), false},
	} {
		if got := r.waitingForConsumer(tc.claim); got != tc.wait {
			t.Errorf("%s: waiting=%v, want %v", tc.name, got, tc.wait)
		}
	}
}

// TestStorageFollowsTheNodeThatWillUseIt covers the translation this exists
// for: the scheduler names a Kubernetes node, and Cinder needs an OpenStack
// zone, which is a different name for the same place.
func TestStorageFollowsTheNodeThatWillUseIt(t *testing.T) {
	known := map[string]Placement{
		"tenant-node-az1": {Zone: "az1", AZ: "nova"},
		"tenant-node-az2": {Zone: "az2", AZ: "zone-b"},
	}
	lookup := func(node string) (Placement, bool) {
		p, ok := known[node]
		return p, ok
	}
	wffc := storagev1.VolumeBindingWaitForFirstConsumer
	base := &Reconciler{
		Classes:     classes(t, class("deferred", &wffc)),
		PlacementOf: lookup,
	}

	// ⚠️ The node decides the Kubernetes zone, and never the OpenStack one.
	// There is no single OpenStack zone namespace: measured on one deployment,
	// Nova and Cinder say "nova" while Manila's share service says
	// "manila-zone-0". Deriving the storage zone from the compute one works
	// only where the names coincide, and where they do not the scheduler
	// reports no capacity -- which reads as a full backend, not a bad name.
	base.Backend = &Backend{}
	got := base.placementFor(claimOf("deferred", "tenant-node-az2"))
	if got.Zone != "az2" {
		t.Errorf("the volume was not pinned to the node's zone: %+v", got)
	}
	if got.AZ != "" {
		t.Errorf("a compute zone reached the storage service: %+v", got)
	}

	// A node this process does not run cannot be resolved; falling back beats
	// guessing.
	if got := base.placementFor(claimOf("deferred", "someone-elses-node")); got.Zone != "" {
		t.Errorf("an unknown node should not produce a zone: %+v", got)
	}

	// An operator naming one storage zone means it, whatever the compute does.
	base.Backend = &Backend{AvailabilityZone: "storage-only"}
	if got := base.placementFor(claimOf("deferred", "tenant-node-az2")); got.AZ != "storage-only" {
		t.Errorf("the deployment setting did not win: %+v", got)
	}
}
