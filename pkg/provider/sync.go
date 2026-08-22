package provider

import (
	"context"
	"strconv"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fivetime/kubezun/pkg/zun"
)

// syncInterval is how often capsule state is reconciled into pod status.
// Zun has no watch API, so this poll is the only source of change; it feeds
// NotifyPods so the node controller itself never has to poll.
const syncInterval = 5 * time.Second

// capsuleAppearGrace is how long a pod may exist before a capsule that cannot
// be read by name is treated as lost rather than as not yet recorded.
const capsuleAppearGrace = 30 * time.Second

// capsuleCreateTimeout is how long a capsule may stay in Creating before the
// pod is failed. A capsule normally reaches Running in a couple of minutes;
// one that never leaves Creating has usually lost its request — a create sent
// while zun-compute was restarting leaves the record in place with no compute
// node ever having seen it, and nothing in Zun retries. Failing the pod lets
// its controller replace it, which is the only thing that recovers.
const capsuleCreateTimeout = 10 * time.Minute

// terminalCapsuleGrace is how long a pod that has finished keeps its capsule.
//
// ⚠️ A capsule holds a Neutron port, and the port holds an address out of the
// tenant's own subnet. Kubernetes keeps terminal pod objects around for
// post-mortem -- PodGC's default threshold is 12500 of them -- so without this
// a workload that crash-loops takes an address out of a /24 on every attempt
// and never gives one back, while the tenant's live pod count stays flat. A
// real kubelet has no such problem: the sandbox is torn down when the pod
// finishes and only the log files outlive it.
//
// The wait is the trade this cannot avoid: `kubectl logs` on a finished pod
// reads the capsule, so reclaiming it immediately would take the post-mortem
// away with the address. An hour is long enough to look, short enough that a
// crash loop cannot exhaust a subnet.
const terminalCapsuleGrace = time.Hour

func (p *Provider) syncLoop(ctx context.Context) {
	t := time.NewTicker(syncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.syncOnce(ctx); err != nil {
				log.G(ctx).WithError(err).Warn("capsule status sync failed")
			}
		}
	}
}

func (p *Provider) syncOnce(ctx context.Context) error {
	p.mu.RLock()
	tracked := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		tracked = append(tracked, pod)
	}
	p.mu.RUnlock()

	// One list per tenant per cycle rather than one read per pod: capsules
	// carry the pod they belong to in their labels, and each tenant's listing
	// is scoped to its own project, so the union covers the whole node.
	managed := map[string]*zun.Capsule{}
	covered := map[string]bool{}
	err := p.capsules.Each(ctx, func(tenant string, api *zun.CapsuleAPI) error {
		part, err := api.ListManaged(ctx)
		if err != nil {
			// ⚠️ Ends the WHOLE cycle. The loop below reads a pod's absence
			// from this map as "its capsule is gone" and fails the pod, so a
			// tenant whose listing failed mid-walk must not leave a partial
			// map behind. (A tenant the walk SKIPPED is different: it is
			// absent from covered, and the judgement below refuses to judge
			// its pods.)
			return err
		}
		covered[tenant] = true
		for k, v := range part {
			managed[k] = v
		}
		return nil
	})
	if err != nil {
		return err
	}

	now := metav1.NewTime(time.Now())
	for _, pod := range tracked {
		// ⚠️ A pod may only be judged against listings that could have seen
		// it. Its tenant's walk being skipped — no usable credential this
		// round — says nothing about its capsule, and reading the empty map
		// as "gone" would fail every pod of that tenant over a credential
		// hiccup. Their status stays as it was until their tenant answers.
		if t, ok := p.capsules.TenantOf(pod.Namespace); !ok || !covered[t] {
			continue
		}
		key := zun.PodKey(pod.Namespace, pod.Name)
		cap, found := managed[key]
		if found && cap.PodUID() != "" && cap.PodUID() != string(pod.UID) {
			// A capsule left behind by an earlier pod of the same name. Reading
			// its status onto this pod would report the dead pod's health, and
			// its address as this pod's — sending traffic somewhere this pod
			// never ran. The orphan sweep removes it; until then this pod has
			// no capsule.
			found = false
		}
		if found && isTerminal(pod) {
			// The pod is finished but its capsule is still there, holding the
			// address. Nothing will come for it: the pod controller ignores
			// terminal pods (virtual-kubelet node/podcontroller.go:620), so
			// even deleting the pod object does not reach DeletePod -- the
			// orphan sweep is what eventually collects it, and only once the
			// object is gone, which may be never.
			p.reclaimFinished(ctx, pod, cap)
			continue
		}
		if !found {
			// A pod already reported terminal has nothing left to reconcile;
			// drop it so provider state does not grow with every deletion.
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				p.mu.Lock()
				delete(p.pods, key)
				delete(p.deleted, key)
				p.mu.Unlock()
				continue
			}
			// Zun records a capsule before it is readable by name, so a pod
			// created moments ago is not yet evidence of a lost capsule;
			// failing it here would make the ReplicaSet churn through pods.
			if pod.Status.StartTime != nil &&
				time.Since(pod.Status.StartTime.Time) < capsuleAppearGrace {
				continue
			}
			// The capsule is gone while the pod is still expected to run.
			if pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning {
				p.updateStatus(pod, func(pod *corev1.Pod) {
					pod.Status.Phase = corev1.PodFailed
					pod.Status.Reason = "ContainerStatusUnknown"
					pod.Status.Conditions = zun.PodConditions("Error", false, now)
					failContainers(pod, "ContainerStatusUnknown",
						"the containers are gone and their outcome cannot be determined", now)
				})
			}
			continue
		}

		// Judged by the capsule's own creation time, not the pod's start
		// time: the latter is set by this process and resets whenever it
		// restarts, so a capsule stuck since before a restart would never age
		// out.
		if cap.Status == "Creating" && !cap.CreatedAt.IsZero() &&
			time.Since(cap.CreatedAt.Time) > capsuleCreateTimeout {
			log.G(ctx).WithField("pod", key).WithField("capsule", cap.UUID).
				Warn("capsule never left Creating; failing the pod so it can be replaced")
			p.updateStatus(pod, func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Reason = "FailedCreatePodSandBox"
				pod.Status.Conditions = zun.PodConditions("Error", false, now)
				failContainers(pod, "FailedCreatePodSandBox",
					"the sandbox never finished being created; no machine claimed this pod", now)
			})
			continue
		}

		phase := zun.PhaseOf(cap)
		// Ready has to satisfy three separate things before traffic is sent
		// here: the capsule runs, it has an address to reach it on, and its
		// containers say they are serving. The last one is the only source
		// that can tell a healthy application from one that merely holds its
		// port open, which is what readiness probes are for.
		ready := cap.Status == "Running" &&
			zun.PodIP(cap) != "" &&
			zun.CapsuleReady(cap)
		statuses := zun.ContainerStatuses(pod, cap)
		ip := zun.PodIP(cap)

		p.updateStatus(pod, func(pod *corev1.Pod) {
			pod.Status.Phase = phase
			pod.Status.Reason = cap.StatusReason
			pod.Status.Conditions = zun.PodConditions(cap.Status, ready, now)
			pod.Status.ContainerStatuses = statuses
			if ip != "" {
				// podIP is the capsule's Neutron port address; keeping them
				// equal is what lets EndpointSlice-driven load balancing and
				// tenant DNS treat capsules like any other pod (DESIGN §5).
				pod.Status.PodIP = ip
				pod.Status.PodIPs = []corev1.PodIP{{IP: ip}}
				pod.Status.HostIP = ip
			}
		})
	}
	return nil
}

// failContainers records a pod-level failure on each container that has not
// started.
//
// The phase alone does not reach the tenant, in either direction. kubectl
// prints a waiting container's reason in preference to the pod phase, so a
// failed pod goes on displaying ContainerCreating with nothing saying why; and
// a container left marked Running keeps the pod reading 1/1 Ready after the
// capsule behind it is gone, which is the false health this whole path exists
// to prevent.
//
// A container that already terminated is left alone: its own exit describes
// what happened better than any pod-level reason could.
func failContainers(pod *corev1.Pod, reason, message string, t metav1.Time) {
	for i := range pod.Status.ContainerStatuses {
		st := &pod.Status.ContainerStatuses[i]
		if st.State.Terminated != nil {
			continue
		}
		st.Ready = false
		st.State = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   1,
				Reason:     reason,
				Message:    message,
				FinishedAt: t,
			},
		}
	}
}

// updateStatus applies a status change and notifies the node controller only
// when something actually changed, so a steady state produces no traffic.
func (p *Provider) updateStatus(pod *corev1.Pod, mutate func(*corev1.Pod)) {
	key := zun.PodKey(pod.Namespace, pod.Name)

	p.mu.Lock()
	current, ok := p.pods[key]
	if !ok {
		p.mu.Unlock()
		return
	}
	before := statusFingerprint(current)
	updated := current.DeepCopy()
	mutate(updated)
	changed := statusFingerprint(updated) != before
	if changed {
		p.pods[key] = updated
	}
	notify := p.notify
	p.mu.Unlock()

	if changed {
		notify(updated)
	}
}

func isTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

// reclaimFinished releases a finished pod's capsule, and with it the address
// the capsule was holding, once the grace period has passed.
//
// The pod's own status already carries the outcome -- exit code, reason, and
// both timestamps -- so nothing a consumer reads is lost with the capsule; the
// logs are, which is what the grace period is for.
func (p *Provider) reclaimFinished(ctx context.Context, pod *corev1.Pod, cap *zun.Capsule) {
	finished := finishedAt(pod)
	if finished.IsZero() || time.Since(finished) < terminalCapsuleGrace {
		return
	}
	capsules, err := p.capsules.For(ctx, pod.Namespace)
	if err != nil {
		return // no credential this round; the next sweep comes back
	}
	key := zun.PodKey(pod.Namespace, pod.Name)
	if err := capsules.Delete(ctx, cap.UUID); err != nil && !errdefs.IsNotFound(err) {
		log.G(ctx).WithField("pod", key).WithField("capsule", cap.UUID).
			WithError(err).Warn("could not release a finished pod's capsule")
		return
	}
	log.G(ctx).WithField("pod", key).WithField("capsule", cap.UUID).
		Info("released a finished pod's capsule and its address")
}

// finishedAt is when the last of a pod's containers stopped. Falls back to the
// pod's start time so a terminal pod whose containers report nothing -- failed
// before any container ran -- still ages out instead of holding an address
// forever.
func finishedAt(pod *corev1.Pod) time.Time {
	var last time.Time
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.FinishedAt.After(last) {
			last = t.FinishedAt.Time
		}
	}
	if last.IsZero() && pod.Status.StartTime != nil {
		last = pod.Status.StartTime.Time
	}
	return last
}

func statusFingerprint(pod *corev1.Pod) string {
	s := string(pod.Status.Phase) + "|" + pod.Status.Reason + "|" + pod.Status.PodIP
	for _, c := range pod.Status.ContainerStatuses {
		// The restart count is part of the fingerprint: a container replaced
		// between two polls can come back Running and ready, and without this
		// the restart would never be written to the pod.
		s += "|" + c.Name + ":" + containerStateKey(c.State) + ":" + boolKey(c.Ready) +
			":" + strconv.Itoa(int(c.RestartCount))
	}
	for _, c := range pod.Status.Conditions {
		s += "|" + string(c.Type) + "=" + string(c.Status)
	}
	return s
}

func containerStateKey(st corev1.ContainerState) string {
	switch {
	// The timestamps are part of the key: a start time can arrive on a later
	// poll than the state itself (the backend learns it lazily), and without
	// it here the status would stay steady-looking and never be re-sent.
	case st.Running != nil:
		return "running@" + strconv.FormatInt(st.Running.StartedAt.Unix(), 10)
	case st.Terminated != nil:
		// The exit code too: 1 and 2 both read "Error", and a reason-only key
		// would swallow the difference a Job's consumer acts on.
		return "terminated:" + st.Terminated.Reason +
			":" + strconv.Itoa(int(st.Terminated.ExitCode)) +
			"@" + strconv.FormatInt(st.Terminated.StartedAt.Unix(), 10)
	case st.Waiting != nil:
		return "waiting:" + st.Waiting.Reason
	}
	return "unknown"
}

func boolKey(b bool) string {
	if b {
		return "t"
	}
	return "f"
}
