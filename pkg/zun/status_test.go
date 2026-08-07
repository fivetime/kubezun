package zun

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStoppedContainerIsTerminatedNotRunning(t *testing.T) {
	// Reporting a stopped container as Running leaves a Job forever
	// incomplete and hides a crash from a Deployment.
	st := ContainerState(&Container{Status: "Stopped"})
	if st.Running != nil {
		t.Fatal("a stopped container was reported as running")
	}
	if st.Terminated == nil {
		t.Fatal("a stopped container has no terminated state")
	}
	if st.Terminated.ExitCode != 0 {
		t.Errorf("clean exit code = %d, want 0", st.Terminated.ExitCode)
	}
}

func TestFailedContainerDoesNotReportSuccess(t *testing.T) {
	// Exit code 0 is what every caller reads as success, so a failed
	// container must not report it.
	for _, status := range []string{"Error", "Dead"} {
		st := ContainerState(&Container{Status: status})
		if st.Terminated == nil {
			t.Fatalf("%s: no terminated state", status)
		}
		if st.Terminated.ExitCode == 0 {
			t.Errorf("%s reported exit code 0, which reads as success", status)
		}
	}
}

func TestStartTimeIsPreserved(t *testing.T) {
	started := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	st := ContainerState(&Container{Status: "Running", StartedAt: Time{started}})
	if st.Running == nil {
		t.Fatal("running container has no running state")
	}
	if !st.Running.StartedAt.Time.Equal(started) {
		t.Errorf("startedAt = %v, want %v", st.Running.StartedAt, started)
	}
}

func TestReadyRequiresTheDataPlane(t *testing.T) {
	// A capsule whose port has no address is running but unreachable;
	// reporting Ready would put it behind a Service and black-hole traffic.
	now := metav1.NewTime(time.Now())
	notReady := conditionByType(PodConditions("Running", false, now), corev1.PodReady)
	if notReady.Status != corev1.ConditionFalse {
		t.Errorf("running capsule without an address is Ready=%v", notReady.Status)
	}
	if notReady.Reason == "" {
		t.Error("not-ready condition gives no reason")
	}
	ready := conditionByType(PodConditions("Running", true, now), corev1.PodReady)
	if ready.Status != corev1.ConditionTrue {
		t.Errorf("running capsule with an address is Ready=%v", ready.Status)
	}
}

func TestPodPhaseMapping(t *testing.T) {
	cases := map[string]corev1.PodPhase{
		"Running":  corev1.PodRunning,
		"Stopped":  corev1.PodSucceeded,
		"Error":    corev1.PodFailed,
		"Dead":     corev1.PodFailed,
		"Creating": corev1.PodPending,
		"Nonsense": corev1.PodUnknown,
	}
	for status, want := range cases {
		if got := PodPhase(status); got != want {
			t.Errorf("PodPhase(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestPodIPPrefersIPv4Address(t *testing.T) {
	cap := &Capsule{Addresses: map[string][]Address{
		"net": {{Version: 4, Addr: "192.168.100.10", Port: "port-1"}},
	}}
	if got := PodIP(cap); got != "192.168.100.10" {
		t.Errorf("PodIP = %q, want the capsule's tenant network address", got)
	}
	if ports := PortIDs(cap); len(ports) != 1 || ports[0] != "port-1" {
		t.Errorf("PortIDs = %v, want [port-1]", ports)
	}
	if got := PodIP(&Capsule{}); got != "" {
		t.Errorf("PodIP of an address-less capsule = %q, want empty", got)
	}
}

func conditionByType(conds []corev1.PodCondition, t corev1.PodConditionType) corev1.PodCondition {
	for _, c := range conds {
		if c.Type == t {
			return c
		}
	}
	return corev1.PodCondition{}
}

func containerWithProbeState(status string, probes, state map[string]any) *Container {
	hc := map[string]any{}
	if probes != nil {
		hc["k8s_probes"] = probes
	}
	if state != nil {
		hc["k8s_probe_state"] = state
	}
	return &Container{Status: status, Healthcheck: hc}
}

func TestReadinessDecidesWhetherTrafficArrives(t *testing.T) {
	readinessDeclared := map[string]any{"readinessProbe": map[string]any{}}

	cases := []struct {
		name string
		c    *Container
		want bool
		why  string
	}{{
		name: "no probe declared",
		c:    containerWithProbeState("Running", nil, nil),
		want: true,
		why:  "with no probe nothing can say the container is not serving",
	}, {
		name: "probe declared but not yet answered",
		c:    containerWithProbeState("Running", readinessDeclared, nil),
		want: false,
		why:  "traffic must not go to a container that has never been checked",
	}, {
		name: "probe passing",
		c: containerWithProbeState("Running", readinessDeclared,
			map[string]any{"ready": true}),
		want: true,
	}, {
		name: "probe failing",
		c: containerWithProbeState("Running", readinessDeclared,
			map[string]any{"ready": false}),
		want: false,
		why:  "this is the split-brain case: the port is open, the data is not",
	}, {
		name: "not running",
		c: containerWithProbeState("Stopped", readinessDeclared,
			map[string]any{"ready": true}),
		want: false,
		why:  "a stale probe result must not outlive the container",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContainerReady(tc.c); got != tc.want {
				t.Errorf("ready = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

func TestCapsuleIsReadyOnlyWhenEveryContainerIs(t *testing.T) {
	declared := map[string]any{"readinessProbe": map[string]any{}}
	serving := containerWithProbeState("Running", declared, map[string]any{"ready": true})
	broken := containerWithProbeState("Running", declared, map[string]any{"ready": false})

	if !CapsuleReady(&Capsule{Containers: []Container{*serving, *serving}}) {
		t.Error("a capsule whose containers all serve was reported not ready")
	}
	if CapsuleReady(&Capsule{Containers: []Container{*serving, *broken}}) {
		t.Error("a capsule with one container not serving was reported ready")
	}
	if CapsuleReady(&Capsule{}) {
		t.Error("a capsule with no containers was reported ready")
	}
}

// A capsule Placement refused never creates its containers, so they sit in
// their initial state forever. kubectl prints a waiting container's reason
// ahead of the pod phase, so the tenant would see "Creating" indefinitely for
// a pod that has definitively failed.
func TestContainerStatusesReportUnschedulableCapsule(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
	}}
	cap := &Capsule{
		Status:       "Error",
		StatusReason: "There are not enough hosts available.",
		Containers:   []Container{{UUID: "u", Status: "Creating"}},
	}
	got := ContainerStatuses(pod, cap)
	w := got[0].State.Waiting
	if w == nil {
		t.Fatalf("container is not waiting: %+v", got[0].State)
	}
	if w.Reason != "CapsuleUnschedulable" {
		t.Errorf("reason = %q, want CapsuleUnschedulable", w.Reason)
	}
	if w.Message != "There are not enough hosts available." {
		t.Errorf("message = %q, want Zun's reason", w.Message)
	}
}

// A capsule that ran on a host and then stopped is a different thing: its
// containers describe what happened, and overwriting that would hide a real
// exit behind a placement message.
func TestContainerStatusesLeavePlacedCapsulesAlone(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
	}}
	cap := &Capsule{
		Status: "Stopped",
		Host:   "incus-node-04",
		// Waiting rather than terminated so the override path is the one under
		// test: a terminated container would never reach it.
		Containers: []Container{{UUID: "u", Status: "Creating"}},
	}
	if w := ContainerStatuses(pod, cap)[0].State.Waiting; w == nil || w.Reason != "Creating" {
		t.Errorf("waiting state was overwritten for a capsule that had a host: %+v", w)
	}
}
