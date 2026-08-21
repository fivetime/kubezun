package provider

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fivetime/kubezun/pkg/zun"
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

// multiTenantCapsules is two tenants, one answering and one skipped — the
// resolver's behaviour when a tenant's credential cannot be used this round.
type multiTenantCapsules struct {
	answering StaticCapsules
}

func (m multiTenantCapsules) For(ctx context.Context, namespace string) (*zun.CapsuleAPI, error) {
	if t, _ := m.TenantOf(namespace); t == "answering" {
		return m.answering.API, nil
	}
	return nil, fmt.Errorf("this tenant has no usable credential")
}

func (m multiTenantCapsules) Each(ctx context.Context, fn func(string, *zun.CapsuleAPI) error) error {
	// The skipped tenant is not visited at all — exactly what
	// ResolvedCapsules.Each does when a credential cannot be resolved.
	return fn("answering", m.answering.API)
}

func (m multiTenantCapsules) TenantOf(namespace string) (string, bool) {
	switch namespace {
	case "answering-ns":
		return "answering", true
	case "skipped-ns":
		return "skipped", true
	}
	return "", false
}

// TestSyncDoesNotJudgeAPodWhoseTenantWasSkipped is the invariant that makes
// skipping a tenant in Each safe at all.
//
// ⚠️ The sync reads a pod's absence from the listings as "its capsule is gone"
// and FAILS the pod. A skipped tenant contributes no listing, so without the
// covered check every one of its pods — all running fine — would be failed and
// replaced over what was only a credential hiccup. Same failure shape as the
// unsynced-cache bug fixed in cff9f8b, one level up.
func TestSyncDoesNotJudgeAPodWhoseTenantWasSkipped(t *testing.T) {
	calls := 0
	p := newTestProvider(t, "answering-ns", "skipped-ns")
	p.capsules = multiTenantCapsules{answering: StaticCapsules{API: countingZun(t, &calls)}}

	// Placed in provider state directly rather than through trackPod: trackPod
	// stamps StartTime with now, which lands the pod inside capsuleAppearGrace
	// and the judgement is never reached — the guard passes untested. Measured:
	// the first version of this test stayed green with the guard removed.
	started := metav1.NewTime(time.Now().Add(-time.Hour)) // far past any grace
	victim := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "skipped-ns", Name: "web", UID: "uid-v"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &started},
	}
	p.mu.Lock()
	p.pods["skipped-ns/web"] = victim
	p.mu.Unlock()

	var failed []string
	p.notify = func(pod *corev1.Pod) {
		if pod.Status.Phase == corev1.PodFailed {
			failed = append(failed, pod.Namespace+"/"+pod.Name)
		}
	}

	if err := p.syncOnce(t.Context()); err != nil {
		t.Fatalf("sync failed outright: %v", err)
	}
	if len(failed) != 0 {
		t.Fatalf("pods of a skipped tenant were failed: %v — their capsules "+
			"were never looked at, only their tenant's credential hiccupped", failed)
	}
	// The other half of the criterion: the answering tenant must actually have
	// been consulted, or this test passes against a sync that does nothing.
	if calls == 0 {
		t.Fatal("the answering tenant was never listed, so the check above proves nothing")
	}
}

// TestFingerprintHearsALateStartTime pins the third hop of the startedAt
// chain: the backend learns a container's start time on a poll AFTER the one
// that reported it running, so the only visible difference between two
// statuses is the timestamp. Measured before this: the Zun API served
// started_at while every pod stayed null — the fingerprint judged the two
// statuses identical and the update was never sent.
func TestFingerprintHearsALateStartTime(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "c",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}}
	before := statusFingerprint(pod)
	pod.Status.ContainerStatuses[0].State.Running.StartedAt =
		metav1.NewTime(time.Date(2026, 8, 16, 17, 43, 13, 0, time.UTC))
	if statusFingerprint(pod) == before {
		t.Fatal("a start time arriving without any other change was invisible " +
			"to the fingerprint; the pod would report startedAt null forever")
	}

	// The exit-code half of the same blindness: 1 and 2 both read "Error".
	term := func(code int32) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "Error", ExitCode: code,
				}},
			}},
		}}
	}
	if statusFingerprint(term(1)) == statusFingerprint(term(2)) {
		t.Fatal("two different exit codes under one reason fingerprint the same")
	}
}
