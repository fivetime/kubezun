package provider

import (
	"context"
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
		cap, found := managed[zun.PodKey(pod.Namespace, pod.Name)]
		if !found {
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
				})
			}
			continue
		}

		phase := zun.PodPhase(cap.Status)
		// Ready means the workload is reachable, which needs the data plane as
		// well as the process: a capsule whose port has no address yet is
		// running but unreachable.
		ready := cap.Status == "Running" && zun.PodIP(cap) != ""
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
		s += "|" + c.Name + ":" + containerStateKey(c.State) + ":" + boolKey(c.Ready)
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
