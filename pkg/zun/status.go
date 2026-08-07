package zun

import (
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
func ContainerState(c *Container) corev1.ContainerState {
	started := metav1.NewTime(c.StartedAt.Time)
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
				FinishedAt: metav1.NewTime(c.UpdatedAt.Time),
			},
		}
	case "Error", "Dead":
		return corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   exitCode(c),
				Reason:     c.Status,
				Message:    c.StatusDetail,
				StartedAt:  started,
				FinishedAt: metav1.NewTime(c.UpdatedAt.Time),
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
func exitCode(c *Container) int32 {
	switch c.Status {
	case "Error", "Dead":
		return 1
	default:
		return 0
	}
}

// ContainerStatuses builds the per-container statuses of a pod from a capsule.
//
// Matching is positional: Zun ignores the name given in the template and
// invents its own ("capsule-<capsule>-<word>-<n>"), so there is nothing to
// match on but the order, which Zun preserves from the template.
func ContainerStatuses(pod *corev1.Pod, cap *Capsule) []corev1.ContainerStatus {
	out := make([]corev1.ContainerStatus, 0, len(pod.Spec.Containers))
	for i, spec := range pod.Spec.Containers {
		st := corev1.ContainerStatus{
			Name:  spec.Name,
			Image: spec.Image,
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
			},
		}
		if i < len(cap.Containers) {
			c := &cap.Containers[i]
			st.State = ContainerState(c)
			st.Ready = ContainerReady(c)
			st.ContainerID = "zun://" + c.UUID
		}
		if w := st.State.Waiting; w != nil {
			if reason, msg, failed := capsuleFailure(cap); failed {
				// A capsule that never reached a host leaves its containers
				// reporting Creating forever. kubectl prints a waiting
				// container's reason in preference to the pod phase, so
				// without this the pod reads as still starting when it has
				// definitively failed — and the reason it failed, which Zun
				// did report, never reaches the tenant.
				w.Reason, w.Message = reason, msg
			}
		}
		out = append(out, st)
	}
	return out
}

// capsuleFailure reports whether the capsule as a whole has failed in a way
// its containers cannot show, and why.
//
// The case that matters is a capsule Placement refused: no host can satisfy
// what it asked for — an architecture none of them run, an availability zone
// with no capacity — so no container was ever created and each one is left in
// whatever pre-start state it was born in.
func capsuleFailure(cap *Capsule) (reason, message string, failed bool) {
	switch cap.Status {
	case "Error", "Dead":
	case "Stopped":
		if cap.Host != "" {
			// It ran somewhere and then stopped; that is not a failure to
			// place, and the containers' own states describe it.
			return "", "", false
		}
	default:
		return "", "", false
	}
	if cap.Host == "" {
		return "CapsuleUnschedulable", capsuleReasonOr(cap,
			"no compute host could satisfy this pod's placement requirements"), true
	}
	return "CapsuleFailed", capsuleReasonOr(cap, "the capsule failed"), true
}

func capsuleReasonOr(cap *Capsule, fallback string) string {
	if cap.StatusReason != "" {
		return cap.StatusReason
	}
	return fallback
}

// ContainerReady reports whether a container should receive traffic.
//
// Running is not enough on its own: an application can hold its port open
// while serving stale data, which is exactly what a readiness probe exists to
// catch — a database mid-failover, a RAFT member on the losing side of a
// split. Where a readiness probe was declared, its answer decides; until it
// has answered once the container is not ready, since assuming otherwise would
// send traffic to something that has never been checked.
func ContainerReady(c *Container) bool {
	if c.Status != "Running" {
		return false
	}
	if c.HasReadinessProbe() && !c.ProbeAnswered() {
		return false
	}
	return c.Ready()
}

// CapsuleReady reports whether every container in a capsule is ready. A pod is
// only ready when all of its containers are.
func CapsuleReady(cap *Capsule) bool {
	if len(cap.Containers) == 0 {
		return false
	}
	for i := range cap.Containers {
		if !ContainerReady(&cap.Containers[i]) {
			return false
		}
	}
	return true
}

// PodIP returns the capsule's address on the tenant network. Keeping podIP
// equal to the Neutron port's IP is what lets the platform's Service and DNS
// controllers treat a capsule exactly like any other endpoint (DESIGN §5).
func PodIP(cap *Capsule) string {
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
func PortIDs(cap *Capsule) []string {
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
