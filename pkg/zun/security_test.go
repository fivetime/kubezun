package zun

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func i64(v int64) *int64 { return &v }
func b(v bool) *bool     { return &v }

// The pod PodSecurity `restricted` forces a tenant to write. Every field it
// demands has to reach Zun, because the one that goes missing is always the one
// that made the container safer.
func TestRestrictedPodCarriesEveryFieldItWasForcedToWrite(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   b(true),
		RunAsUser:      i64(65532),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	pod.Spec.Containers = []corev1.Container{{
		Name: "app",
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: b(false),
			ReadOnlyRootFilesystem:   b(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}}

	built, err := buildContainer(pod, pod.Spec.Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	if built.Security == nil {
		t.Fatal("the container carries no security context")
	}
	app := *built.Security
	if app.RunAsUser == nil || *app.RunAsUser != 65532 {
		t.Error("runAsUser did not survive, so the container would run as the image's user")
	}
	if app.ReadOnlyRootFilesystem == nil || !*app.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem did not survive, so the root filesystem stays writable")
	}
	if app.AllowPrivilegeEscalation == nil || *app.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation: false did not survive")
	}
	if app.Capabilities == nil || len(app.Capabilities.Drop) != 1 {
		t.Error("dropped capabilities did not survive, so nothing is dropped")
	}
	if app.SeccompProfile == nil {
		t.Error("the seccomp profile did not survive")
	}
}

// A container's own setting wins over the pod's; what it leaves unset it
// inherits. Getting this backwards would silently loosen one of the two.
func TestContainerOverridesPod(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: i64(1000)}
	pod.Spec.Containers = []corev1.Container{
		{Name: "own", SecurityContext: &corev1.SecurityContext{RunAsUser: i64(2000)}},
		{Name: "inherits"},
	}

	own, err := buildContainer(pod, pod.Spec.Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	inherits, err := buildContainer(pod, pod.Spec.Containers[1])
	if err != nil {
		t.Fatal(err)
	}
	if *own.Security.RunAsUser != 2000 {
		t.Errorf("container's own runAsUser lost: %d", *own.Security.RunAsUser)
	}
	if *inherits.Security.RunAsUser != 1000 {
		t.Errorf("pod's runAsUser not inherited: %d", *inherits.Security.RunAsUser)
	}
}

// runAsNonRoot with no uid cannot be honoured here, and accepting it would run
// the container as whatever the image says — the outcome it exists to prevent.
func TestRunAsNonRootWithoutUserIsRefused(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: b(true)}
	pod.Spec.Containers = []corev1.Container{{Name: "app"}}

	err := validateSecurity(pod)
	if err == nil {
		t.Fatal("accepted runAsNonRoot with no uid to send")
	}
	if !strings.Contains(err.Error(), "runAsUser") {
		t.Errorf("the refusal does not say what to do about it: %v", err)
	}
}

func TestPrivilegedStillRefused(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Spec.Containers = []corev1.Container{{
		Name:            "app",
		SecurityContext: &corev1.SecurityContext{Privileged: b(true)},
	}}
	if err := validateSecurity(pod); err == nil {
		t.Fatal("a privileged container was accepted")
	}
}

// A pod that asks for nothing produces no annotation, rather than an empty one
// for the driver to parse.
func TestNoSecurityContextNoAnnotation(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Spec.Containers = []corev1.Container{{Name: "app"}}
	built, err := buildContainer(pod, pod.Spec.Containers[0])
	if err != nil {
		t.Fatal(err)
	}
	if built.Security != nil {
		t.Error("sent a security context for a pod that asked for nothing")
	}
}
