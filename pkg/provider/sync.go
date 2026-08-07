package provider

import (
	"context"
	"strconv"
	"time"

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

	// One list per cycle rather than one read per pod: capsules carry the pod
	// they belong to in their labels, so a single call covers the whole node.
	managed, err := p.capsules.ListManaged(ctx)
	if err != nil {
		return err
	}

	now := metav1.NewTime(time.Now())
	for _, pod := range tracked {
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
					pod.Status.Reason = "CapsuleMissing"
					pod.Status.Conditions = zun.PodConditions("Error", false, now)
					failContainers(pod, "CapsuleMissing",
						"the capsule backing this pod no longer exists", now)
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
				pod.Status.Reason = "CapsuleStuckCreating"
				pod.Status.Conditions = zun.PodConditions("Error", false, now)
				failContainers(pod, "CapsuleStuckCreating",
					"the capsule never left Creating; no compute node ever claimed it", now)
			})
			continue
		}

		phase := zun.PodPhase(cap.Status)
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
	case st.Running != nil:
		return "running"
	case st.Terminated != nil:
		return "terminated:" + st.Terminated.Reason
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
