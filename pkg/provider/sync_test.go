package provider

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A pod failed by the sync loop must say so on its containers too. kubectl
// prints a waiting container's reason ahead of the pod phase, so without this a
// pod that has definitively failed goes on displaying ContainerCreating with
// nothing anywhere explaining why.
func TestFailContainersReplacesWaitingState(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "waiting", State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
		{Name: "running", Ready: true, State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{}}},
		{Name: "exited", State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 137}}},
	}}}

	failContainers(pod, "FailedCreatePodSandBox", "no machine claimed this pod",
		metav1.NewTime(time.Now()))

	got := pod.Status.ContainerStatuses[0]
	if got.State.Terminated == nil || got.State.Terminated.Reason != "FailedCreatePodSandBox" {
		t.Errorf("waiting container was not failed: %+v", got.State)
	}
	if got.State.Terminated != nil && got.State.Terminated.ExitCode == 0 {
		t.Error("a failed container reported exit code 0, which callers read as success")
	}
	// A container still marked Running would keep the pod reading Ready after
	// the capsule behind it is gone — the false health this path exists to stop.
	running := pod.Status.ContainerStatuses[1]
	if running.State.Running != nil {
		t.Error("a container was left Running though its capsule is gone")
	}
	if running.Ready {
		t.Error("a container of a failed pod is still marked ready")
	}

	// One that already exited described its own fate; that is better than any
	// pod-level reason, so it is left alone.
	if exited := pod.Status.ContainerStatuses[2]; exited.State.Terminated.ExitCode != 137 {
		t.Errorf("an exited container's own status was overwritten: %+v", exited.State.Terminated)
	}
}
