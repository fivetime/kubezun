// Package provider implements the virtual-kubelet provider that runs pods as
// OpenStack Zun capsules.
package provider

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"github.com/fivetime/kubezun/pkg/zun"
)

// Config describes what one tenant's virtual node runs.
type Config struct {
	// Namespaces the node serves. Every entry point checks incoming pods
	// against this set: the pod controller only filters on spec.nodeName,
	// which anyone able to create a pod can write, so this check — not any
	// admission policy — is the boundary that keeps one tenant's pods off
	// another tenant's node (DESIGN §4).
	Namespaces []string

	// NetworkID is the tenant Neutron network capsules attach to.
	NetworkID string

	// AvailabilityZone maps this node's topology zone onto Zun.
	AvailabilityZone string

	// NodeName is the name this provider's node registered under.
	NodeName string
}

// Provider runs pods as Zun capsules for a single tenant.
type Provider struct {
	cfg        Config
	namespaces map[string]struct{}
	capsules   *zun.CapsuleAPI

	mu   sync.RWMutex
	pods map[string]*corev1.Pod // key: namespace/name

	notify func(*corev1.Pod)
}

// New builds a provider for one tenant.
func New(cfg Config, client *zun.Client) (*Provider, error) {
	if len(cfg.Namespaces) == 0 {
		return nil, fmt.Errorf("at least one namespace must be served")
	}
	ns := make(map[string]struct{}, len(cfg.Namespaces))
	for _, n := range cfg.Namespaces {
		ns[n] = struct{}{}
	}
	return &Provider{
		cfg:        cfg,
		namespaces: ns,
		capsules:   zun.NewCapsuleAPI(client),
		pods:       make(map[string]*corev1.Pod),
		notify:     func(*corev1.Pod) {},
	}, nil
}

// authorize rejects work for a namespace this node does not serve. It returns
// a not-found error rather than a forbidden one so a caller probing for other
// tenants' pods cannot tell an unauthorized namespace from an empty one.
func (p *Provider) authorize(namespace string) error {
	if _, ok := p.namespaces[namespace]; !ok {
		return errdefs.NotFoundf("namespace %q is not served by node %s",
			namespace, p.cfg.NodeName)
	}
	return nil
}

// CreatePod converts a pod into a capsule and creates it.
func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) (err error) {
	defer recoverAs(&err, "CreatePod")
	if err := p.authorize(pod.Namespace); err != nil {
		return err
	}

	tpl, err := zun.BuildTemplate(pod, zun.TemplateOptions{
		NetworkID:        p.cfg.NetworkID,
		AvailabilityZone: p.cfg.AvailabilityZone,
	})
	if err != nil {
		return err
	}

	log.G(ctx).WithField("pod", zun.PodKey(pod.Namespace, pod.Name)).Info("creating capsule")
	if _, err := p.capsules.Create(ctx, tpl); err != nil {
		return err
	}

	p.trackPod(pod, corev1.PodPending, "Creating")
	return nil
}

// UpdatePod is a no-op: a capsule's spec cannot be changed in place, and
// recreating it here would drop the pod's IP and restart every container
// behind the caller's back.
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	if err := p.authorize(pod.Namespace); err != nil {
		return err
	}
	log.G(ctx).WithField("pod", zun.PodKey(pod.Namespace, pod.Name)).
		Debug("ignoring pod update: capsules are immutable")
	return nil
}

// DeletePod removes the capsule backing a pod. It does not wait for the
// capsule to disappear: the caller holds a worker while this runs, and Zun
// reports the terminal state through the status poll instead.
func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) (err error) {
	defer recoverAs(&err, "DeletePod")
	if err := p.authorize(pod.Namespace); err != nil {
		return err
	}

	key := zun.PodKey(pod.Namespace, pod.Name)
	log.G(ctx).WithField("pod", key).Info("deleting capsule")
	if err := p.capsules.Delete(ctx, zun.CapsuleName(pod)); err != nil && !errdefs.IsNotFound(err) {
		return err
	}

	// The pod stays in provider state, marked terminated, rather than being
	// dropped here: the node controller refuses to remove a pod object whose
	// status still says running, so it has to observe the terminal status
	// first. The sync loop drops the entry once the capsule is gone.
	//
	// The pod handed in is used when this provider has no record of it, which
	// is the case for every pod already terminating when the process started:
	// without this the deletion would never be reported and the pod would hang
	// in Terminating forever.
	now := metav1.NewTime(time.Now())
	p.mu.Lock()
	tracked, ok := p.pods[key]
	if !ok {
		tracked = pod
	}
	terminated := tracked.DeepCopy()
	terminated.Status.Phase = corev1.PodSucceeded
	terminated.Status.Reason = "CapsuleDeleted"
	terminated.Status.Conditions = zun.PodConditions("Deleted", false, now)
	if len(terminated.Status.ContainerStatuses) == 0 {
		for _, c := range terminated.Spec.Containers {
			terminated.Status.ContainerStatuses = append(
				terminated.Status.ContainerStatuses,
				corev1.ContainerStatus{Name: c.Name, Image: c.Image})
		}
	}
	for i := range terminated.Status.ContainerStatuses {
		terminated.Status.ContainerStatuses[i].Ready = false
		terminated.Status.ContainerStatuses[i].State = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "CapsuleDeleted",
				FinishedAt: now,
			},
		}
	}
	p.pods[key] = terminated
	notify := p.notify
	p.mu.Unlock()

	notify(terminated)
	return nil
}

// GetPod returns the pod as this provider last observed it.
func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	if err := p.authorize(namespace); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[zun.PodKey(namespace, name)]
	if !ok {
		return nil, errdefs.NotFoundf("pod %s/%s is not running on this node", namespace, name)
	}
	return pod.DeepCopy(), nil
}

// GetPodStatus returns the status of a pod.
func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return &pod.Status, nil
}

// GetPods lists the pods running on this node.
func (p *Provider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		out = append(out, pod.DeepCopy())
	}
	return out, nil
}

// NotifyPods registers the callback used to push status changes. Without it
// the node falls back to polling every pod on a fixed interval, which scales
// with the number of tenants rather than with the rate of change.
func (p *Provider) NotifyPods(ctx context.Context, cb func(*corev1.Pod)) {
	p.mu.Lock()
	p.notify = cb
	p.mu.Unlock()
	go p.syncLoop(ctx)
}

func (p *Provider) trackPod(pod *corev1.Pod, phase corev1.PodPhase, reason string) {
	now := metav1.NewTime(time.Now())
	tracked := pod.DeepCopy()
	tracked.Status.Phase = phase
	tracked.Status.Reason = reason
	tracked.Status.StartTime = &now
	tracked.Status.Conditions = zun.PodConditions(reason, false, now)
	tracked.Status.ContainerStatuses = make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		tracked.Status.ContainerStatuses = append(tracked.Status.ContainerStatuses,
			corev1.ContainerStatus{
				Name:  c.Name,
				Image: c.Image,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
				},
			})
	}

	p.mu.Lock()
	p.pods[zun.PodKey(pod.Namespace, pod.Name)] = tracked
	p.mu.Unlock()
	p.notify(tracked)
}

// --- Not supported yet; see DESIGN §6 and the fork's TODO. ---

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	if err := p.authorize(namespace); err != nil {
		return nil, err
	}
	return nil, errNotImplemented(
		"container logs need the capsule log endpoint, which this Zun build does not serve yet")
}

func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	if err := p.authorize(namespace); err != nil {
		return err
	}
	return errNotImplemented(
		"exec needs the capsule ExecSync endpoint, which this Zun build does not serve yet")
}

func (p *Provider) AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) error {
	if err := p.authorize(namespace); err != nil {
		return err
	}
	return errNotImplemented("attach is not supported on a KNaaS virtual node")
}

func (p *Provider) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) error {
	if err := p.authorize(namespace); err != nil {
		return err
	}
	return errNotImplemented("port-forward is not supported on a KNaaS virtual node")
}

func (p *Provider) GetStatsSummary(ctx context.Context) (*statsv1alpha1.Summary, error) {
	return nil, errNotImplemented("stats are not collected on a KNaaS virtual node")
}

func (p *Provider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	return nil, errNotImplemented("metrics are not collected on a KNaaS virtual node")
}

// errNotImplemented reports a capability this node does not serve. The
// message names what is missing so an operator reading an event knows whether
// to wait for a Zun feature or to stop expecting the call to work at all.
func errNotImplemented(msg string) error { return fmt.Errorf("not implemented: %s", msg) }

// recoverAs turns a panic into an error. A malformed pod must fail its own
// creation, not take down the node and with it every other pod of this tenant.
func recoverAs(err *error, op string) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%s panicked: %v", op, r)
	}
}
