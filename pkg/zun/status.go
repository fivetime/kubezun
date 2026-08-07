package zun

import (
	"github.com/gophercloud/gophercloud/v2/openstack/container/v1/capsules"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Zun capsule and container statuses:
// Error, Running, Stopped, Paused, Unknown, Creating, Created, Deleted,
// Deleting, Rebuilding, Dead, Restarting.

// PodPhase maps a capsule status onto a pod phase.
func PodPhase(status string) corev1.PodPhase {
	switch status {
	case "Running":
		return corev1.PodRunning
	case "Stopped":
		return corev1.PodSucceeded
	case "Error", "Dead":
		return corev1.PodFailed
	case "Creating", "Created", "Restarting", "Rebuilding", "Paused",
		"Deleting", "Deleted":
		return corev1.PodPending
	}
	return corev1.PodUnknown
}

// PodConditions reports the conditions for a capsule status. ready tells
// whether the data plane is usable as well: a capsule whose Neutron port has
// not reached ACTIVE is running but unreachable, and reporting Ready then
// would send traffic into a black hole.
func PodConditions(status string, ready bool, t metav1.Time) []corev1.PodCondition {
	scheduled := corev1.PodCondition{
		Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: t,
	}
	switch status {
	case "Running":
		readyStatus := corev1.ConditionFalse
		reason := "NetworkNotReady"
		if ready {
			readyStatus = corev1.ConditionTrue
			reason = ""
		}
		return []corev1.PodCondition{
			{Type: corev1.PodReady, Status: readyStatus, Reason: reason, LastTransitionTime: t},
			{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: t},
			scheduled,
		}
	case "Creating", "Created", "Rebuilding", "Restarting":
		return []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: status, LastTransitionTime: t},
			{Type: corev1.PodInitialized, Status: corev1.ConditionFalse, Reason: status, LastTransitionTime: t},
			scheduled,
		}
	}
	return []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: status, LastTransitionTime: t},
		scheduled,
	}
}

// ContainerState maps a capsule container onto a Kubernetes container state.
func ContainerState(c *capsules.Container) corev1.ContainerState {
	started := metav1.NewTime(c.StartedAt)
	switch c.Status {
	case "Running":
		return corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: started},
		}
	case "Stopped":
		// Stopped is a terminal state, not a running one: a container that
		// exited must not be reported as Running or a Job never completes and
		// a Deployment never restarts it.
		return corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   exitCode(c),
				Reason:     "Completed",
				Message:    c.StatusDetail,
				StartedAt:  started,
				FinishedAt: metav1.NewTime(c.UpdatedAt),
			},
		}
	case "Error", "Dead":
		return corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   exitCode(c),
				Reason:     c.Status,
				Message:    c.StatusDetail,
				StartedAt:  started,
				FinishedAt: metav1.NewTime(c.UpdatedAt),
			},
		}
	default:
		return corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  c.Status,
				Message: c.StatusReason,
			},
		}
	}
}

// exitCode reports the container's exit status. Zun does not expose the real
// code, so a failed container must not claim 0: that is the code callers read
// as success.
func exitCode(c *capsules.Container) int32 {
	switch c.Status {
	case "Error", "Dead":
		return 1
	default:
		return 0
	}
}

// ContainerStatuses builds the per-container statuses of a pod from a capsule.
func ContainerStatuses(pod *corev1.Pod, cap *capsules.Capsule) []corev1.ContainerStatus {
	byName := make(map[string]*capsules.Container, len(cap.Containers))
	for i := range cap.Containers {
		byName[cap.Containers[i].Name] = &cap.Containers[i]
	}

	out := make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))
	for _, spec := range pod.Spec.Containers {
		st := corev1.ContainerStatus{
			Name:  spec.Name,
			Image: spec.Image,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
			},
		}
		// Zun prefixes container names with "capsule-<capsule name>-"; match
		// on suffix so a rename on either side does not silently drop status.
		for name, c := range byName {
			if name == spec.Name || hasSuffixName(name, spec.Name) {
				st.State = ContainerState(c)
				st.Ready = c.Status == "Running"
				st.ContainerID = "zun://" + c.UUID
				break
			}
		}
		out = append(out, st)
	}
	return out
}

func hasSuffixName(full, name string) bool {
	if len(full) <= len(name) {
		return false
	}
	return full[len(full)-len(name):] == name
}

// PodIP returns the capsule's address on the tenant network. Keeping podIP
// equal to the Neutron port's IP is what lets the platform's Service and DNS
// controllers treat a capsule exactly like any other endpoint (DESIGN §5).
func PodIP(cap *capsules.Capsule) string {
	for _, addrs := range cap.Addresses {
		for _, a := range addrs {
			if a.Version == 4 && a.Addr != "" {
				return a.Addr
			}
		}
	}
	return ""
}

// PortIDs lists the Neutron ports backing a capsule.
func PortIDs(cap *capsules.Capsule) []string {
	var ids []string
	for _, addrs := range cap.Addresses {
		for _, a := range addrs {
			if a.Port != "" {
				ids = append(ids, a.Port)
			}
		}
	}
	return ids
}
