package vknode

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

// recordingProvider notes which pods it was asked to create.
type recordingProvider struct {
	mu      sync.Mutex
	created []string
}

func (p *recordingProvider) CreatePod(_ context.Context, pod *corev1.Pod) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created = append(p.created, pod.Name)
	return nil
}
func (p *recordingProvider) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.created...)
}

func (p *recordingProvider) UpdatePod(context.Context, *corev1.Pod) error { return nil }
func (p *recordingProvider) DeletePod(context.Context, *corev1.Pod) error { return nil }
func (p *recordingProvider) GetPod(context.Context, string, string) (*corev1.Pod, error) {
	return nil, errdefs.NotFound("no pod")
}
func (p *recordingProvider) GetPodStatus(context.Context, string, string) (*corev1.PodStatus, error) {
	return nil, errdefs.NotFound("no pod")
}
func (p *recordingProvider) GetPods(context.Context) ([]*corev1.Pod, error) { return nil, nil }
func (p *recordingProvider) GetContainerLogs(context.Context, string, string, string, api.ContainerLogOpts) (io.ReadCloser, error) {
	return nil, errdefs.NotFound("no logs")
}
func (p *recordingProvider) RunInContainer(context.Context, string, string, string, []string, api.AttachIO) error {
	return errdefs.NotFound("no exec")
}
func (p *recordingProvider) AttachToContainer(context.Context, string, string, string, api.AttachIO) error {
	return errdefs.NotFound("no attach")
}
func (p *recordingProvider) GetStatsSummary(context.Context) (*statsv1alpha1.Summary, error) {
	return &statsv1alpha1.Summary{}, nil
}
func (p *recordingProvider) GetMetricsResource(context.Context) ([]*dto.MetricFamily, error) {
	return nil, nil
}
func (p *recordingProvider) PortForward(context.Context, string, string, int32, io.ReadWriteCloser) error {
	return errdefs.NotFound("no port forward")
}

// staticNodeProvider keeps a node's status as given.
type staticNodeProvider struct{}

func (staticNodeProvider) Ping(context.Context) error                           { return nil }
func (staticNodeProvider) NotifyNodeStatus(context.Context, func(*corev1.Node)) {}

func nodeObject(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func podOn(namespace, name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID("uid-" + name),
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
		},
	}
}

// The whole point of sharing one informer is that it carries every node's pods.
// Each node's controller must therefore act only on its own, or a tenant's arm64
// node would create capsules for its amd64 node's pods — on the wrong machines,
// twice over, and each node's orphan sweep would then fight the other's.
func TestEachNodeHandlesOnlyItsOwnPods(t *testing.T) {
	client := fake.NewSimpleClientset()
	set, err := NewSet(SetOptions{Client: client, Namespaces: []string{"t1-default"}})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	amd, arm := &recordingProvider{}, &recordingProvider{}
	for name, p := range map[string]*recordingProvider{
		"t1-node-az1": amd, "t1-node-arm64": arm,
	} {
		if _, err := set.AddNode(NodeOptions{
			Spec:         nodeObject(name),
			Provider:     p,
			NodeProvider: staticNodeProvider{},
		}); err != nil {
			t.Fatalf("AddNode %s: %v", name, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go set.Run(ctx) //nolint:errcheck

	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	if err := set.WaitReady(readyCtx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}

	for _, pod := range []*corev1.Pod{
		podOn("t1-default", "on-amd", "t1-node-az1"),
		podOn("t1-default", "on-arm", "t1-node-arm64"),
		podOn("t1-default", "on-neither", "some-real-worker"),
	} {
		if _, err := client.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", pod.Name, err)
		}
	}

	waitFor(t, func() bool { return len(amd.names()) > 0 && len(arm.names()) > 0 })

	if got := amd.names(); len(got) != 1 || got[0] != "on-amd" {
		t.Errorf("amd64 node created %v, want only on-amd", got)
	}
	if got := arm.names(); len(got) != 1 || got[0] != "on-arm" {
		t.Errorf("arm64 node created %v, want only on-arm", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			// Give any wrongly-routed pod a moment to arrive too, so the test
			// fails on over-delivery rather than passing on a race.
			time.Sleep(500 * time.Millisecond)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for both nodes to handle a pod")
}

func TestNewSetRequiresClientAndNamespaces(t *testing.T) {
	if _, err := NewSet(SetOptions{Namespaces: []string{"t1"}}); err == nil {
		t.Error("a set without a client was accepted")
	}
	if _, err := NewSet(SetOptions{Client: fake.NewSimpleClientset()}); err == nil {
		t.Error("a set serving no namespace was accepted")
	}
}

func TestAddNodeRequiresANodeProvider(t *testing.T) {
	// Defaulting to virtual-kubelet's naive provider would hold the node Ready
	// whatever happened to Zun, so the scheduler would keep sending pods to a
	// node that cannot create a capsule.
	set, err := NewSet(SetOptions{Client: fake.NewSimpleClientset(), Namespaces: []string{"t1"}})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if _, err := set.AddNode(NodeOptions{
		Spec: nodeObject("t1-node"), Provider: &recordingProvider{},
	}); err == nil {
		t.Error("a node without a node provider was accepted")
	}
	if _, err := set.AddNode(NodeOptions{
		Spec: nodeObject("t1-node"), NodeProvider: staticNodeProvider{},
	}); err == nil {
		t.Error("a node without a provider was accepted")
	}
	if _, err := set.AddNode(NodeOptions{
		Provider: &recordingProvider{}, NodeProvider: staticNodeProvider{},
	}); err == nil {
		t.Error("a node without a spec was accepted")
	}
}
