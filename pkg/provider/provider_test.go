package provider

import (
	"context"
	"testing"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func newTestProvider(t *testing.T, namespaces ...string) *Provider {
	t.Helper()
	p, err := New(Config{Namespaces: namespaces, NodeName: "111111-node-az1"}, nil)
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
	if _, err := New(Config{NodeName: "n"}, nil); err == nil {
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
