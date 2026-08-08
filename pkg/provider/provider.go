// Package provider implements the virtual-kubelet provider that runs pods as
// OpenStack Zun capsules.
package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
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

	// Architecture is the machine this node's capsules run on, matching the
	// node's kubernetes.io/arch label.
	Architecture string

	// NodeName is the name this provider's node registered under.
	NodeName string

	// ClusterDomain is the suffix Service names resolve under. It becomes the
	// pods' resolver search list, which is what lets an application written for
	// Kubernetes use a Service's short name.
	ClusterDomain string
}

// Provider runs pods as Zun capsules for a single tenant.
type Provider struct {
	cfg        Config
	namespaces map[string]struct{}
	capsules   *zun.CapsuleAPI

	// podLister is the cluster's view of pods, used to tell a capsule whose
	// pod is gone from one whose pod this process simply has not seen yet.
	podLister corev1listers.PodLister

	// configMaps and secrets back the pod's file volumes. A capsule has no
	// kubelet to project them, so their content is read here and sent with it.
	configMaps corev1listers.ConfigMapLister
	secrets    corev1listers.SecretLister

	mu   sync.RWMutex
	pods map[string]*corev1.Pod // key: namespace/name
	// deleted holds pods kept only so their terminal status can be reported.
	// The node controller refuses to remove a pod object whose status still
	// says running, so the record has to outlive the capsule — but it must not
	// be presented as a live pod, or a pod recreated under the same name would
	// be mistaken for it. Values are the deleted pod's UID.
	deleted map[string]types.UID

	notify func(*corev1.Pod)
}

// Caches are the informer-backed views a provider reads. Pods may be nil, which
// disables orphan cleanup; without ConfigMaps and Secrets a pod with a file
// volume is refused rather than started without its files.
type Caches struct {
	Pods       corev1listers.PodLister
	ConfigMaps corev1listers.ConfigMapLister
	Secrets    corev1listers.SecretLister
}

// New builds a provider for one tenant.
func New(cfg Config, client *zun.Client, caches Caches) (*Provider, error) {
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
		podLister:  caches.Pods,
		configMaps: caches.ConfigMaps,
		secrets:    caches.Secrets,
		capsules:   zun.NewCapsuleAPI(client),
		pods:       make(map[string]*corev1.Pod),
		deleted:    make(map[string]types.UID),
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

	key := zun.PodKey(pod.Namespace, pod.Name)

	// Zun does not reject a second capsule under an existing name, so a
	// retried create — the node controller retries whenever anything after
	// this fails — would leave a duplicate running and billed for every
	// attempt. Check before creating rather than relying on the API to refuse.
	if existing, err := p.capsules.ListManaged(ctx); err == nil {
		if c, ok := existing[key]; ok && c.PodUID() == string(pod.UID) {
			log.G(ctx).WithField("pod", key).WithField("capsule", c.Name()).
				Info("capsule already exists for this pod; not creating another")
			p.trackPod(pod, corev1.PodPending, "Creating")
			return nil
		}
	}

	files, err := p.resolveFiles(pod)
	if err != nil {
		return err
	}

	searches, nameservers := dnsConfigFor(pod, p.cfg.ClusterDomain)

	tpl, err := zun.BuildTemplate(pod, zun.TemplateOptions{
		NetworkID:        p.cfg.NetworkID,
		AvailabilityZone: p.cfg.AvailabilityZone,
		Architecture:     p.cfg.Architecture,
		NodeName:         p.cfg.NodeName,
		Files:            files,
		DNSSearches:      searches,
		DNSNameservers:   nameservers,
	})
	if err != nil {
		return err
	}

	log.G(ctx).WithField("pod", key).Info("creating capsule")
	if _, err := p.capsules.Create(ctx, tpl); err != nil {
		return err
	}

	p.trackPod(pod, corev1.PodPending, "Creating")
	return nil
}

// UpdatePod is a no-op for a pod that already has its capsule: a capsule's spec
// cannot be changed in place, and recreating it here would drop the pod's IP
// and restart every container behind the caller's back.
//
// It is not a no-op when the pod is a different pod that reused a name. The
// node controller keys its work by namespace/name, so when a pod is deleted and
// one of the same name is created before the deletion is processed — routine
// for a StatefulSet, whose pod names are stable across restarts — the new pod
// arrives here as an update. Treating that as nothing to do would leave it
// Pending forever with no capsule and no error to explain it.
func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	if err := p.authorize(pod.Namespace); err != nil {
		return err
	}
	key := zun.PodKey(pod.Namespace, pod.Name)

	p.mu.RLock()
	tracked, known := p.pods[key]
	p.mu.RUnlock()

	if known && tracked.UID == pod.UID {
		log.G(ctx).WithField("pod", key).
			Debug("ignoring pod update: capsules are immutable")
		return nil
	}
	if pod.DeletionTimestamp != nil {
		// On its way out; creating a capsule now would leave one behind.
		return nil
	}

	log.G(ctx).WithField("pod", key).WithField("uid", pod.UID).
		Info("pod reused a name this node was still tracking; creating its capsule")
	return p.CreatePod(ctx, pod)
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
	p.deleted[key] = terminated.UID
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
	key := zun.PodKey(namespace, name)

	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[key]
	if !ok {
		return nil, errdefs.NotFoundf("pod %s/%s is not running on this node", namespace, name)
	}
	if uid, gone := p.deleted[key]; gone && uid == pod.UID {
		// Kept only to report its terminal status. Answering with it would let
		// the node controller compare a pod recreated under this name against
		// the dead one — and their specs match, so it would conclude there is
		// nothing to do and the new pod would never get a capsule. A
		// StatefulSet reuses pod names on every restart, so this is the normal
		// case, not a corner one.
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
	if p.podLister != nil {
		go p.orphanLoop(ctx)
	} else {
		log.G(ctx).Warn("orphan cleanup disabled: no pod cache was provided")
	}
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

	key := zun.PodKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	p.pods[key] = tracked
	delete(p.deleted, key)
	p.mu.Unlock()
	p.notify(tracked)
}

// --- Not supported yet; see DESIGN §6 and the fork's TODO. ---

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	if err := p.authorize(namespace); err != nil {
		return nil, err
	}
	p.mu.RLock()
	pod, tracked := p.pods[zun.PodKey(namespace, podName)]
	p.mu.RUnlock()
	if !tracked {
		return nil, errdefs.NotFoundf("pod %s/%s is not running on this node", namespace, podName)
	}
	if opts.Follow {
		// Zun returns the whole log at once rather than a stream. Polling for
		// more would repeat lines at every boundary, so this says what it
		// cannot do instead of appearing to follow.
		return nil, errNotImplemented("following logs needs a streaming endpoint Zun does not serve")
	}

	logOpts := zun.LogOptions{
		Tail:       opts.Tail,
		Timestamps: opts.Timestamps,
	}
	if opts.SinceSeconds > 0 {
		logOpts.Since = time.Now().Add(-time.Duration(opts.SinceSeconds) * time.Second)
	}
	if !opts.SinceTime.IsZero() {
		logOpts.Since = opts.SinceTime
	}

	index := containerIndex(pod, containerName)
	if index < 0 {
		return nil, errdefs.NotFoundf(
			"pod %s/%s has no container named %s", namespace, podName, containerName)
	}

	data, err := p.capsules.LogsForPod(ctx, string(pod.UID), index, logOpts)
	if err != nil {
		return nil, err
	}
	if opts.LimitBytes > 0 && len(data) > opts.LimitBytes {
		// Kubernetes truncates from the start, keeping the most recent output,
		// which is what a caller asking for a limit is looking for.
		data = data[len(data)-opts.LimitBytes:]
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// dnsConfigFor decides what the pod's resolver is told: which servers to ask,
// and what to append to a short name.
//
// The gateway writes both into the pod as the tenant sees them — searches under
// the namespace the tenant knows rather than the one this cluster stores, and
// the address of that tenant's own resolver. Composing them here instead would
// produce a namespace no application will ever ask for, because a tenant whose
// namespace is "default" is stored as "<tenant>-default" and writes the former.
//
// It falls back to composing them when the pod carries none. The gateway leaves
// the config empty while the tenant's resolver is not yet serving, and a pod
// created in that window with no search list at all resolves nothing by short
// name.
func dnsConfigFor(pod *corev1.Pod, clusterDomain string) (searches, nameservers []string) {
	if cfg := pod.Spec.DNSConfig; cfg != nil && len(cfg.Searches) > 0 {
		return cfg.Searches, cfg.Nameservers
	}
	return composeSearches(pod.Namespace, clusterDomain), nil
}

// composeSearches is the fallback: the three entries a kubelet would compose.
// It uses the namespace this cluster stores, which for a tenant behind the
// gateway is not the one their manifests name — hence only a fallback.
func composeSearches(namespace, clusterDomain string) []string {
	base := strings.TrimSuffix(clusterDomain, ".")
	if base == "" {
		return nil
	}
	base = strings.TrimPrefix(base, "svc.")
	return []string{
		namespace + ".svc." + base,
		"svc." + base,
		base,
	}
}

// containerIndex reports where a container sits in the pod's spec, which is
// how it is identified on the Zun side.
func containerIndex(pod *corev1.Pod, name string) int {
	for i, c := range pod.Spec.Containers {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	if err := p.authorize(namespace); err != nil {
		return err
	}
	p.mu.RLock()
	pod, tracked := p.pods[zun.PodKey(namespace, podName)]
	p.mu.RUnlock()
	if !tracked {
		return errdefs.NotFoundf("pod %s/%s is not running on this node", namespace, podName)
	}
	if attach != nil && attach.TTY() {
		// Zun runs the command to completion and answers with everything at
		// once. There is no stream to attach a terminal to, and pretending
		// otherwise would leave a caller waiting at a prompt that never reads.
		return errNotImplemented(
			"exec with a terminal needs a streaming endpoint Zun does not serve; " +
				"run the command without -t")
	}

	index := containerIndex(pod, containerName)
	if index < 0 {
		return errdefs.NotFoundf(
			"pod %s/%s has no container named %s", namespace, podName, containerName)
	}

	result, err := p.capsules.Exec(ctx, string(pod.UID), index, cmd)
	if err != nil {
		return err
	}
	if attach != nil {
		if out := attach.Stdout(); out != nil {
			if _, err := io.WriteString(out, result.Output); err != nil {
				return err
			}
		}
	}
	if result.ExitCode != 0 {
		// The caller has to see this: kubectl reports a command's exit status,
		// and swallowing it would make a failed command look successful.
		return fmt.Errorf("command terminated with exit code %d", result.ExitCode)
	}
	return nil
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
