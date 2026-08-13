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

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"

	"github.com/fivetime/kubezun/pkg/netpol"
	"github.com/fivetime/kubezun/pkg/service"
	"github.com/fivetime/kubezun/pkg/zun"
)

// Config describes what one tenant's virtual node runs.
type Config struct {
	// ServesNamespace reports whether this node runs pods from a namespace.
	// Every entry point asks it: the pod controller only filters on
	// spec.nodeName, which anyone able to create a pod can write, so this
	// check — not any admission policy — is the boundary that keeps one
	// tenant's pods off another tenant's node (DESIGN §4).
	//
	// A function rather than a fixed set because a tenant creates namespaces
	// while this is running, and a set fixed at startup means a namespace made
	// afterwards has no compute: its pods stay Pending forever, silently, since
	// authorize deliberately cannot distinguish "not served" from "empty".
	ServesNamespace func(namespace string) bool

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

	// Tenant is the gateway's prefix on this tenant's namespaces. A capsule's
	// search list has to name namespaces the way the tenant's own resolver
	// serves them -- the resolver watches through the gateway and so knows
	// "default", never "111111-default".
	Tenant string

	// ClusterDNS pins the resolver capsules are given, the way kubelet's
	// --cluster-dns does. Left empty, it is looked up from DNSService, which is
	// the usual case: the address is an Octavia VIP that does not exist until
	// this process builds it.
	ClusterDNS []string

	// DNSService names the Service whose address is the tenant's resolver, as
	// "namespace/name" in the tenant's own terms (kube-system/kube-dns).
	DNSService string

	// Tokens mints ServiceAccount tokens for pods that ask for one.
	Tokens TokenMinter

	// ResolveClaim turns a persistentVolumeClaim volume into the storage
	// behind it. Nil means this node serves no claims, and a pod naming one is
	// refused rather than started without its data.
	ResolveClaim func(namespace, claim string) (zun.ClaimMount, error)
}

// Provider runs pods as Zun capsules for a single tenant.
type Provider struct {
	cfg      Config
	capsules *zun.CapsuleAPI

	// podLister is the cluster's view of pods, used to tell a capsule whose
	// pod is gone from one whose pod this process simply has not seen yet.
	podLister corev1listers.PodLister
	// podsSynced guards the orphan sweep against an unsynced podLister. See
	// Caches.PodsSynced.
	podsSynced func() bool

	// objects backs the pod's file volumes. A capsule has no kubelet to project
	// them, so their content is read here and sent with it.
	objects ObjectReader

	mu   sync.RWMutex
	pods map[string]*corev1.Pod // key: namespace/name
	// deleted holds pods kept only so their terminal status can be reported.
	// The node controller refuses to remove a pod object whose status still
	// says running, so the record has to outlive the capsule — but it must not
	// be presented as a live pod, or a pod recreated under the same name would
	// be mistaken for it. Values are the deleted pod's UID.
	deleted map[string]types.UID

	notify func(*corev1.Pod)

	// unowned remembers the capsules the orphan sweep has already said it
	// cannot judge, so it says so once each rather than every two minutes.
	unowned map[string]struct{}

	// policies decides which security groups a pod's port carries, from the
	// NetworkPolicies that select it. Nil on a deployment that does not
	// enforce them, which leaves every capsule on the project default.
	policies *netpol.Reconciler

	// cpuRates remembers each container's last CPU counter so the next reading
	// becomes a rate. Zun reports a cumulative count, as the runtime does.
	cpuRates *rates

	// tokens mints a ServiceAccount token bound to one pod. Nil means this
	// node hands out none, and a pod that wants one is refused rather than
	// started without the credential its application will look for.
	tokens TokenMinter
	// tokenExpiries is when each pod's token runs out, guarded by mu.
	tokenExpiries map[string]tokenExpiry
}

// ObjectReader reads one object at a time.
//
// Deliberately not a lister. A lister is backed by a cache of every object of
// its kind the watch covers, and this process serves one tenant: a cache wide
// enough to answer for every namespace the tenant may create is also wide
// enough to hold every other tenant's Secrets, which is a copy of their
// credentials sitting in memory that nothing this process does needs. Volumes
// are read once, when a capsule is built, so there is nothing to cache for.
type ObjectReader interface {
	ConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, error)
	Secret(ctx context.Context, namespace, name string) (*corev1.Secret, error)
	Service(ctx context.Context, namespace, name string) (*corev1.Service, error)
}

// Caches are the views a provider reads. Pods may be nil, which disables orphan
// cleanup; without Objects a pod with a file volume is refused rather than
// started without its files.
type Caches struct {
	Pods    corev1listers.PodLister
	Objects ObjectReader

	// PodsSynced reports whether Pods has finished its initial list. ⚠️ This is
	// load-bearing, not hygiene: an informer that has been started but has not
	// listed yet answers NotFound for every pod, and the orphan sweep reads
	// NotFound as "the pod is gone". The library hands the provider its
	// callback before it waits for that sync (podcontroller.go:306 registers,
	// :312 waits), so the sweep would be looking at an empty cache with every
	// capsule on the node judged against it. On a restart every capsule is
	// past the grace period, so the first sweep would delete the whole node's
	// workload -- and a slow API server, which is what makes the sync take
	// long enough to matter, is also what makes the restart happen.
	//
	// Nil means "assume synced", which is what the tests want and what a
	// caller passing a static lister means.
	PodsSynced func() bool
}

// New builds a provider for one tenant.
//
// It takes the capsule API rather than the session it was built from: that is
// all this needs, and asking for the session would make every caller build the
// same endpoint again.
func New(cfg Config, capsules *zun.CapsuleAPI, caches Caches) (*Provider, error) {
	if cfg.ServesNamespace == nil {
		return nil, fmt.Errorf("a namespace check is required")
	}
	return &Provider{
		cfg:        cfg,
		podLister:  caches.Pods,
		podsSynced: caches.PodsSynced,
		objects:    caches.Objects,
		capsules:   capsules,
		pods:       make(map[string]*corev1.Pod),
		deleted:    make(map[string]types.UID),
		notify:     func(*corev1.Pod) {},
		cpuRates:   newRates(),
		tokens:     cfg.Tokens,

		tokenExpiries: make(map[string]tokenExpiry),
	}, nil
}

// authorize rejects work for a namespace this node does not serve. It returns
// a not-found error rather than a forbidden one so a caller probing for other
// tenants' pods cannot tell an unauthorized namespace from an empty one.
func (p *Provider) authorize(namespace string) error {
	if !p.cfg.ServesNamespace(namespace) {
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
				Info("capsule already exists for this pod; adopting it")
			p.adoptPod(pod)
			return nil
		}
	}

	files, err := p.resolveFiles(ctx, pod)
	if err != nil {
		return err
	}

	searches, nameservers := p.dnsConfigFor(ctx, pod)

	claims, err := p.resolveClaims(pod)
	if err != nil {
		return err
	}

	// ⚠️ Decided before the capsule exists, not attached to it afterwards. A
	// tenant's pods reach each other because they share a security group, not
	// because they share a network, so a capsule created with the wrong groups
	// is reachable by the wrong people for as long as it takes to correct --
	// and a capsule created with none is given the project's permissive
	// default, which no policy can then take back.
	groups, err := p.securityGroupsFor(ctx, pod)
	if err != nil {
		return err
	}

	tpl, err := zun.BuildTemplate(pod, zun.TemplateOptions{
		SecurityGroups:   groups,
		NetworkID:        p.cfg.NetworkID,
		AvailabilityZone: p.cfg.AvailabilityZone,
		Architecture:     p.cfg.Architecture,
		NodeName:         p.cfg.NodeName,
		Files:            files,
		DNSSearches:      searches,
		DNSNameservers:   nameservers,
		Claims:           claims,
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
	terminated.Status.Reason = "Completed"
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
				// Deleted, which is what happened and what this code did.
				//
				// ⚠️ ContainerStatusUnknown was here, on the reasoning that the
				// container's own exit was never observed. That is true and it
				// is not what a reader needs: the outcome is not unknown, the
				// thing was removed because its pod was going away, and saying
				// "cannot be determined" hides a fact this process is certain
				// of. Same mistake as reporting a paused container as unknown,
				// made in a second place.
				Reason:     "Deleted",
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
	if p.tokens != nil {
		go p.tokenLoop(ctx)
	}
	if p.podLister != nil {
		go p.orphanLoop(ctx)
	} else {
		log.G(ctx).Warn("orphan cleanup disabled: no pod cache was provided")
	}
}

// adoptPod takes over a pod whose capsule is already running, keeping the
// status it already has.
//
// This is what every pod on the node goes through when the process restarts:
// the in-memory map starts empty, so the node controller believes nothing is
// running and calls CreatePod for each, which finds the capsule and lands
// here. Resetting the status to Pending/ContainerCreating -- what this used to
// do -- publishes "not ready" for every pod on the node, and the EndpointSlice
// controller does the obvious thing with that: it empties the Services. The
// Service and Ingress reconcilers then write empty member sets, and the
// tenant's traffic stops. Seconds later the sync loop reads the capsules and
// puts it all back, so the only trace is a dip nobody was watching for.
//
// The status here is the one the API server already holds, which is the last
// thing this process published before it stopped -- accurate until proven
// otherwise, and the sync loop proves it either way within a few seconds.
func (p *Provider) adoptPod(pod *corev1.Pod) {
	key := zun.PodKey(pod.Namespace, pod.Name)
	adopted := pod.DeepCopy()
	if adopted.Status.Phase == "" {
		// Nothing was ever published for it: a capsule made by a create that
		// died before it could record one. Pending is right, and there is no
		// readiness to lose.
		now := metav1.NewTime(time.Now())
		adopted.Status.Phase = corev1.PodPending
		adopted.Status.Reason = "Creating"
		adopted.Status.Conditions = zun.PodConditions("Creating", false, now)
	}

	p.mu.Lock()
	p.pods[key] = adopted
	delete(p.deleted, key)
	p.mu.Unlock()
	// Deliberately not notified: nothing changed. The sync loop publishes the
	// first reading that differs from what is already there.
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

	if opts.Follow {
		// Zun has no streaming endpoint, so this reads the log repeatedly and
		// emits only what is new. Exact rather than approximate: see
		// followLogs for why the runtime's per-line timestamps make it so.
		return p.followLogs(ctx, string(pod.UID), index, logOpts), nil
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

// UsePolicies turns on NetworkPolicy enforcement, and gives the reconciler the
// two things only the provider knows: where a pod's port is, and that this
// provider is the one placing it.
//
// ⚠️ Enabling this on a tenant whose capsules already exist is not a per-pod
// change. A tenant's pods reach each other because every one of them is in the
// project's default group, whose only ingress rule admits members of that same
// group; a pod moved out of it stops being accepted by every pod still in it,
// and the failure lands on the receiver. So the groups have to be added
// everywhere first and the default removed everywhere second (DESIGN §7.7.5a).
func (p *Provider) UsePolicies(r *netpol.Reconciler) {
	if r == nil {
		return
	}
	r.PortOf = p.portOf
	p.policies = r
}

// portOf finds the Neutron port behind a pod's capsule.
//
// Read from the capsule rather than remembered here: the capsule is where the
// address actually lives, and a cache of it would go stale exactly when a
// capsule is rebuilt -- which is when the port matters most.
func (p *Provider) portOf(pod *corev1.Pod) string {
	capsule, err := p.capsules.Get(context.Background(), "kubezun-"+string(pod.UID))
	if err != nil {
		return ""
	}
	for _, addresses := range capsule.Addresses {
		for _, a := range addresses {
			if a.Port != "" {
				return a.Port
			}
		}
	}
	return ""
}

// securityGroupsFor is what this pod's port should carry.
//
// Nil when no policy controller is running, which leaves the field out of the
// template entirely and lets Zun fall through to the project default -- the
// behaviour of every deployment before NetworkPolicy was enforced at all. ⚠️
// That is not the same as an empty list, which means "reach nothing"; the two
// must not be conflated anywhere along this path.
func (p *Provider) securityGroupsFor(ctx context.Context, pod *corev1.Pod) ([]string, error) {
	if p.policies == nil {
		return nil, nil
	}
	groups, err := p.policies.GroupsFor(ctx, pod)
	if err != nil {
		// Refusing to place the pod is the safe direction. Creating it with
		// the default group would give a pod its policy says is isolated the
		// most permissive port the project has.
		return nil, fmt.Errorf("deciding the security groups for %s/%s: %w",
			pod.Namespace, pod.Name, err)
	}
	return groups, nil
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
// dnsConfigFor decides what a capsule's resolver is told, following the pod's
// dnsPolicy the way a kubelet would.
//
// Getting the nameserver here is not a detail: without it a capsule falls back
// to whatever the Neutron subnet hands out, which is a public resolver that has
// never heard of the cluster domain. Every in-cluster name then fails, and
// nothing in the DNS path looks wrong -- the tenant's CoreDNS is running,
// answering, and serving correct records that no capsule ever asks it for.
func (p *Provider) dnsConfigFor(ctx context.Context, pod *corev1.Pod) (searches, nameservers []string) {
	cfg := pod.Spec.DNSConfig

	switch pod.Spec.DNSPolicy {
	case corev1.DNSNone:
		// The pod took the decision itself; give it exactly what it asked for.
		if cfg != nil {
			return cfg.Searches, cfg.Nameservers
		}
		return nil, nil
	case corev1.DNSDefault:
		// "Whatever the infrastructure resolves with" -- here the subnet's.
		return nil, nil
	}

	// ClusterFirst, and the zero value, which is what an unset dnsPolicy
	// arrives as.
	searches = composeSearches(p.tenantNamespace(pod.Namespace), p.cfg.ClusterDomain)
	nameservers = p.clusterDNS(ctx)
	if cfg != nil {
		searches = append(searches, cfg.Searches...)
		if len(cfg.Nameservers) > 0 {
			nameservers = cfg.Nameservers
		}
	}
	return searches, nameservers
}

// tenantNamespace turns the namespace this cluster stores into the one the
// tenant wrote. The gateway prefixes every namespace with the tenant id, and
// the tenant's resolver watches through the gateway -- so it serves
// "web.default.svc.cluster.local" and knows nothing of "111111-default".
// Searching the stored name yields NXDOMAIN for every Service the tenant has.
func (p *Provider) tenantNamespace(namespace string) string {
	if p.cfg.Tenant == "" {
		return namespace
	}
	return strings.TrimPrefix(namespace, p.cfg.Tenant+"-")
}

// clusterDNS resolves the address capsules are given as their resolver: the
// configured one, else the tenant Service's.
//
// The Service's own clusterIP is not it. That address is the gateway's fiction
// for the tenant's benefit and nothing on the tenant network routes it; the
// address that answers is the load balancer this process built, which the
// Service carries in the cluster-ip annotation (the contract in pkg/service).
// A capsule handed the clusterIP times out on every lookup.
func (p *Provider) clusterDNS(ctx context.Context) []string {
	if len(p.cfg.ClusterDNS) > 0 {
		return p.cfg.ClusterDNS
	}
	if p.cfg.DNSService == "" || p.objects == nil {
		return nil
	}
	ns, name, ok := strings.Cut(p.cfg.DNSService, "/")
	if !ok {
		return nil
	}
	if p.cfg.Tenant != "" {
		ns = p.cfg.Tenant + "-" + ns
	}
	svc, err := p.objects.Service(ctx, ns, name)
	if err != nil {
		// A pod created before the resolver's load balancer exists gets the
		// subnet's resolver rather than no pod at all. Its controller replaces
		// it soon enough, and by then the address is there.
		log.G(ctx).WithError(err).WithField("service", p.cfg.DNSService).
			Warn("no resolver address for this capsule: the DNS Service could not be read")
		return nil
	}
	if addr := svc.Annotations[service.AddressAnnotation]; addr != "" {
		return []string{addr}
	}
	log.G(ctx).WithField("service", p.cfg.DNSService).
		Warn("the DNS Service has no reachable address yet; this capsule gets the subnet's resolver")
	return nil
}

// composeSearches builds the three entries a kubelet would, against the
// namespace as the tenant knows it.
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
	index := containerIndex(pod, containerName)
	if index < 0 {
		return errdefs.NotFoundf(
			"pod %s/%s has no container named %s", namespace, podName, containerName)
	}

	if attach != nil && attach.TTY() {
		// A terminal is a session, not an answer: the command has not
		// finished and the point is to type into it. Served on the runtime's
		// own stream, through the proxy on the node holding the capsule.
		return p.runInteractive(ctx, string(pod.UID), index, cmd, attach)
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

// resolveClaims maps each of the pod's claim volumes to the storage behind it.
//
// Resolution failure fails the pod, loudly, before anything is created. A pod
// started without its volume runs -- and writes into its own image, which
// looks like it worked until the capsule is replaced and everything written is
// gone. That is the least traceable failure storage can produce, and the one
// this refuses to be.
func (p *Provider) resolveClaims(pod *corev1.Pod) (map[string]zun.ClaimMount, error) {
	var out map[string]zun.ClaimMount
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if v.PersistentVolumeClaim == nil {
			continue
		}
		if p.cfg.ResolveClaim == nil {
			return nil, fmt.Errorf(
				"volume %s: this node was started without persistent volume support", v.Name)
		}
		m, err := p.cfg.ResolveClaim(pod.Namespace, v.PersistentVolumeClaim.ClaimName)
		if err != nil {
			return nil, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if out == nil {
			out = make(map[string]zun.ClaimMount)
		}
		out[v.Name] = m
	}
	return out, nil
}
