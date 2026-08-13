package vknode

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func svc(ns, name string) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func waitScoped(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(t.Context(), 20*time.Millisecond, 5*time.Second, true,
		func(context.Context) (bool, error) { return cond(), nil }); err != nil {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestScopedListerSpansServedNamespacesOnly is the point of the whole file:
// both tenants' objects visible, an unserved namespace's invisible — the 6×
// over-caching this replaces would have shown t3's object here.
func TestScopedListerSpansServedNamespacesOnly(t *testing.T) {
	client := fake.NewSimpleClientset(svc("t1", "a"), svc("t2", "b"), svc("t3", "hidden"))
	s := NewScopedFactories(client, 0)
	s.Track([]string{"t1", "t2"}, nil)
	s.Start(t.Context())
	waitScoped(t, "initial sync", s.HasSynced)

	got, err := s.ServiceLister().List(labels.Everything())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly the served namespaces' services, got %d", len(got))
	}
	for _, g := range got {
		if g.Namespace == "t3" {
			t.Fatal("an unserved namespace's object reached the lister — " +
				"the over-caching this exists to remove")
		}
	}
	// The namespaced path too, including the empty answer for the unserved.
	if _, err := s.ServiceLister().Services("t1").Get("a"); err != nil {
		t.Fatalf("a served object was not readable: %v", err)
	}
	if _, err := s.ServiceLister().Services("t3").Get("hidden"); err == nil {
		t.Fatal("an unserved namespace answered a Get")
	}
}

// TestRemovedNamespaceLeavesTheReadPath is the departed-tenant guarantee: no
// purge step exists to forget, because removal IS the purge.
func TestRemovedNamespaceLeavesTheReadPath(t *testing.T) {
	client := fake.NewSimpleClientset(svc("t1", "a"), svc("t2", "b"))
	s := NewScopedFactories(client, 0)
	s.Track([]string{"t1", "t2"}, nil)
	s.Start(t.Context())
	waitScoped(t, "initial sync", s.HasSynced)

	s.Track(nil, []string{"t2"})
	got, _ := s.ServiceLister().List(labels.Everything())
	for _, g := range got {
		if g.Namespace == "t2" {
			t.Fatal("a removed namespace's objects lingered in the read path — " +
				"a departed tenant would keep appearing in peer sets")
		}
	}
	if len(got) != 1 {
		t.Fatalf("the surviving namespace lost its objects too: %d", len(got))
	}
}

// TestLateNamespaceDoesNotUnsyncTheSet: a tenant onboarding must not stall
// every controller that already waited for sync.
func TestLateNamespaceDoesNotUnsyncTheSet(t *testing.T) {
	client := fake.NewSimpleClientset(svc("t1", "a"))
	s := NewScopedFactories(client, 0)
	s.Track([]string{"t1"}, nil)
	s.Start(t.Context())
	waitScoped(t, "initial sync", s.HasSynced)

	// ⚠️ Checked immediately after Track, before the new factory can possibly
	// have listed: this is the moment a strict "all namespaces synced" answer
	// flips false and stalls controllers.
	s.Track([]string{"t2"}, nil)
	if !s.HasSynced() {
		t.Fatal("a joining namespace flipped HasSynced back to false; " +
			"every waiting controller would stall on tenant onboarding")
	}
	// And the late namespace does become visible.
	waitScoped(t, "late namespace visible", func() bool {
		_, err := s.ServiceLister().Services("t2").Get("late")
		if err != nil {
			// object created after Track; make it now and keep polling
			_, _ = client.CoreV1().Services("t2").Create(t.Context(), svc("t2", "late"), metav1.CreateOptions{})
		}
		return err == nil
	})
}

// TestHandlersHearNamespacesAddedLater: a controller subscribes once; events
// must arrive from factories created afterwards, or Services in onboarded
// tenants would never enqueue.
func TestHandlersHearNamespacesAddedLater(t *testing.T) {
	client := fake.NewSimpleClientset(svc("t2", "pre-existing"))
	s := NewScopedFactories(client, 0)
	heard := make(chan string, 8)
	s.OnServices(cache.ResourceEventHandlerFuncs{AddFunc: func(obj any) {
		if o, ok := obj.(*corev1.Service); ok {
			heard <- o.Namespace + "/" + o.Name
		}
	}})
	s.Track([]string{"t1"}, nil)
	s.Start(t.Context())

	s.Track([]string{"t2"}, nil) // joins after subscription and after Start
	select {
	case got := <-heard:
		if got != "t2/pre-existing" {
			t.Fatalf("heard %q, expected the late namespace's object", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event from a namespace added after subscription — " +
			"onboarded tenants' Services would never enqueue")
	}
}
