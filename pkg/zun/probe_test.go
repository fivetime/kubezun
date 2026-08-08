package zun

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// A distroless image is the case this exists for, and the property that matters
// is that the rewritten command names nothing that has to be in the image. No
// shell, no curl, no wget, no nc — only the helper the compute node mounts.
func TestRewrittenProbesNeedNothingFromTheImage(t *testing.T) {
	c := &corev1.Container{
		Name:  "app",
		Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8181}},
	}
	probes := map[string]*corev1.Probe{
		"httpGet": {ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromString("http")},
		}},
		"tcpSocket": {ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(8181)},
		}},
	}

	for kind, p := range probes {
		got, err := RewriteProbe(p, c)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if got.Exec == nil || len(got.Exec.Command) == 0 {
			t.Fatalf("%s was not rewritten into an exec", kind)
		}
		if got.Exec.Command[0] != ProbeHelper {
			t.Errorf("%s runs %q, which the image may not have; want %s",
				kind, got.Exec.Command[0], ProbeHelper)
		}
		for _, arg := range got.Exec.Command {
			for _, tool := range []string{"sh", "curl", "wget", "nc"} {
				if arg == tool {
					t.Errorf("%s still depends on %s from the image", kind, tool)
				}
			}
		}
	}
}

// An exec probe is the tenant's own command and is left exactly as written.
func TestExecProbesAreNotTouched(t *testing.T) {
	p := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		Exec: &corev1.ExecAction{Command: []string{"/app/health"}},
	}}
	got, err := RewriteProbe(p, &corev1.Container{Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Exec.Command[0] != "/app/health" {
		t.Errorf("an exec probe was rewritten: %v", got.Exec.Command)
	}
}
