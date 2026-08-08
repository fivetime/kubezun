package provider

import (
	"context"
	"testing"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/fivetime/kubezun/pkg/zun"
)

func newTestProvider(t *testing.T, namespaces ...string) *Provider {
	t.Helper()
	served := make(map[string]struct{}, len(namespaces))
	for _, n := range namespaces {
		served[n] = struct{}{}
	}
	p, err := New(Config{
		ServesNamespace: func(ns string) bool { _, ok := served[ns]; return ok },
		NodeName:        "111111-node-az1",
	}, nil, Caches{})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return p
}

func testPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID("uid-" + name)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx:alpine"}},
		},
	}
}

func TestNewRequiresNamespaces(t *testing.T) {
	// A node serving every namespace could be reached by any pod that names it
	// in spec.nodeName, which is the escape this list exists to close.
	if _, err := New(Config{NodeName: "n"}, nil, Caches{}); err == nil {
		t.Fatal("a provider without a namespace list was accepted")
	}
}

func TestForeignNamespaceIsRefusedOnEveryEntryPoint(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	ctx := context.Background()
	foreign := testPod("222222-default", "web")

	if err := p.CreatePod(ctx, foreign); !errdefs.IsNotFound(err) {
		t.Errorf("CreatePod: got %v, want a not-found error", err)
	}
	if err := p.UpdatePod(ctx, foreign); !errdefs.IsNotFound(err) {
		t.Errorf("UpdatePod: got %v, want a not-found error", err)
	}
	if err := p.DeletePod(ctx, foreign); !errdefs.IsNotFound(err) {
		t.Errorf("DeletePod: got %v, want a not-found error", err)
	}
	if _, err := p.GetPod(ctx, "222222-default", "web"); !errdefs.IsNotFound(err) {
		t.Errorf("GetPod: got %v, want a not-found error", err)
	}
	if _, err := p.GetPodStatus(ctx, "222222-default", "web"); !errdefs.IsNotFound(err) {
		t.Errorf("GetPodStatus: got %v, want a not-found error", err)
	}
	if _, err := p.GetContainerLogs(ctx, "222222-default", "web", "app", api.ContainerLogOpts{}); !errdefs.IsNotFound(err) {
		t.Errorf("GetContainerLogs: got %v, want a not-found error", err)
	}
	if err := p.RunInContainer(ctx, "222222-default", "web", "app", nil, nil); !errdefs.IsNotFound(err) {
		t.Errorf("RunInContainer: got %v, want a not-found error", err)
	}
}

func TestForeignPodsStayInvisible(t *testing.T) {
	// A refused namespace must be indistinguishable from an empty one, so a
	// caller cannot use the error to discover which tenants exist.
	p := newTestProvider(t, "111111-default")
	served, err := p.GetPod(context.Background(), "111111-default", "absent")
	if served != nil || !errdefs.IsNotFound(err) {
		t.Fatalf("served namespace, absent pod: got %v %v", served, err)
	}
	_, foreignErr := p.GetPod(context.Background(), "222222-default", "absent")
	if !errdefs.IsNotFound(foreignErr) {
		t.Fatalf("foreign namespace: got %v, want the same not-found kind", foreignErr)
	}
}

func TestTrackedPodsAreListedAndCopied(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	p.trackPod(testPod("111111-default", "web"), corev1.PodPending, "Creating")

	pods, err := p.GetPods(context.Background())
	if err != nil || len(pods) != 1 {
		t.Fatalf("GetPods: %v %v", pods, err)
	}
	// Mutating what a caller received must not corrupt provider state.
	pods[0].Status.Phase = corev1.PodFailed
	again, _ := p.GetPod(context.Background(), "111111-default", "web")
	if again.Status.Phase != corev1.PodPending {
		t.Errorf("provider state was mutated through a returned pod: %v", again.Status.Phase)
	}
}

func TestNotifyFiresOnlyWhenStatusChanges(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	var calls int
	p.notify = func(*corev1.Pod) { calls++ }
	p.trackPod(testPod("111111-default", "web"), corev1.PodPending, "Creating")
	calls = 0

	// Same value: no notification, otherwise a steady state would generate
	// one status write per pod per poll.
	p.updateStatus(p.pods["111111-default/web"], func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodPending
	})
	if calls != 0 {
		t.Errorf("unchanged status notified %d times", calls)
	}

	p.updateStatus(p.pods["111111-default/web"], func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodRunning
		pod.Status.PodIP = "192.168.100.10"
	})
	if calls != 1 {
		t.Errorf("changed status notified %d times, want 1", calls)
	}
}

// The node controller keys work by namespace/name, so a pod deleted and
// recreated under the same name — every StatefulSet restart — can reach the
// provider as an update rather than a create. Ignoring it would leave the new
// pod Pending forever with no capsule and nothing to explain it.
func TestUpdatePodCreatesCapsuleForARecreatedPod(t *testing.T) {
	p := newTestProvider(t, "111111-default")

	old := testPod("111111-default", "web")
	old.UID = "uid-old"
	p.trackPod(old, corev1.PodRunning, "Running")

	replacement := testPod("111111-default", "web")
	replacement.UID = "uid-new"

	// CreatePod talks to Zun, so the call fails here; what matters is that the
	// update was routed to it rather than silently dropped.
	err := p.UpdatePod(t.Context(), replacement)
	if err == nil {
		t.Fatal("expected UpdatePod to attempt a create for the recreated pod")
	}

	// The same pod must still be a no-op, or every status change would recreate
	// the capsule and restart the workload.
	same := old.DeepCopy()
	if err := p.UpdatePod(t.Context(), same); err != nil {
		t.Fatalf("update of an unchanged pod should be a no-op, got %v", err)
	}
}

// A pod on its way out must not get a capsule: nothing would ever delete it.
func TestUpdatePodIgnoresTerminatingPods(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	pod := testPod("111111-default", "web")
	pod.UID = "uid-new"
	now := metav1.NewTime(time.Now())
	pod.DeletionTimestamp = &now

	if err := p.UpdatePod(t.Context(), pod); err != nil {
		t.Fatalf("terminating pod should be ignored, got %v", err)
	}
}

// A deleted pod's record is kept only to report its terminal status. The node
// controller asks GetPod before deciding whether a pod needs creating, and it
// compares specs — which are identical for a pod recreated under the same name.
// Answering with the dead pod makes it conclude there is nothing to do, and the
// new pod never gets a capsule. StatefulSets reuse names on every restart.
func TestGetPodHidesADeletedPodFromItsReplacement(t *testing.T) {
	p := newTestProvider(t, "111111-default")
	pod := testPod("111111-default", "web")
	pod.UID = "uid-old"
	p.trackPod(pod, corev1.PodRunning, "Running")

	// Zun is unreachable in tests, so the capsule delete fails; record the
	// deletion the way DeletePod does once it succeeds.
	p.mu.Lock()
	p.deleted[zun.PodKey("111111-default", "web")] = pod.UID
	p.mu.Unlock()

	if _, err := p.GetPod(t.Context(), "111111-default", "web"); !errdefs.IsNotFound(err) {
		t.Fatalf("GetPod returned a deleted pod: err=%v", err)
	}

	// Once a replacement is tracked, it is a live pod again.
	replacement := testPod("111111-default", "web")
	replacement.UID = "uid-new"
	p.trackPod(replacement, corev1.PodPending, "Creating")

	got, err := p.GetPod(t.Context(), "111111-default", "web")
	if err != nil {
		t.Fatalf("GetPod on the replacement: %v", err)
	}
	if got.UID != "uid-new" {
		t.Errorf("GetPod returned UID %q, want uid-new", got.UID)
	}
}
