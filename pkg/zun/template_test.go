package zun

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func pod(mutate ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "111111-default", UID: "abc-123",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx:alpine"}},
		},
	}
	for _, m := range mutate {
		m(p)
	}
	return p
}

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("template is not valid JSON: %v", err)
	}
	return out
}

func TestCapsuleNameUsesUID(t *testing.T) {
	// Two pods sharing a name in different namespaces must not collide: a
	// collision would let one tenant address another tenant's capsule.
	a := CapsuleName(pod())
	b := CapsuleName(pod(func(p *corev1.Pod) {
		p.Namespace = "222222-default"
		p.UID = "def-456"
	}))
	if a == b {
		t.Fatalf("capsule names collide: %q", a)
	}
	if !strings.Contains(a, "abc-123") {
		t.Fatalf("capsule name %q does not carry the pod UID", a)
	}
}

func TestBuildTemplateCarriesNetworkAndLabels(t *testing.T) {
	raw, err := BuildTemplate(pod(), TemplateOptions{
		NetworkID: "net-1", AvailabilityZone: "az1",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	tpl := decode(t, raw)

	nets, ok := tpl["nets"].([]any)
	if !ok || len(nets) != 1 {
		t.Fatalf("template carries no nets: %v", tpl["nets"])
	}
	if got := nets[0].(map[string]any)["network"]; got != "net-1" {
		t.Errorf("network = %v, want net-1", got)
	}
	if got := tpl["availabilityZone"]; got != "az1" {
		t.Errorf("availabilityZone = %v, want az1", got)
	}

	labels := tpl["metadata"].(map[string]any)["labels"].(map[string]any)
	if labels[LabelManagedBy] != ManagedByValue {
		t.Errorf("capsule is not labelled as managed by this provider: %v", labels)
	}
	if labels[LabelNamespace] != "111111-default" || labels[LabelPodName] != "web" {
		t.Errorf("capsule does not carry its pod identity: %v", labels)
	}
}

func TestBuildTemplatePinsPortOverNetwork(t *testing.T) {
	raw, err := BuildTemplate(pod(), TemplateOptions{NetworkID: "net-1", PortID: "port-9"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	nets := decode(t, raw)["nets"].([]any)[0].(map[string]any)
	if nets["port"] != "port-9" {
		t.Errorf("port = %v, want port-9", nets["port"])
	}
	if _, ok := nets["network"]; ok {
		t.Error("a pinned port must not also request a network")
	}
}

func TestBuildTemplateMapsLimitsNotRequests(t *testing.T) {
	// The CRI driver turns the value into a cgroup hard limit, so sending the
	// request would let a container be killed below the ceiling it was promised.
	p := pod(func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}
	})
	raw, err := BuildTemplate(p, TemplateOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := decode(t, raw)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	req := c["resources"].(map[string]any)["requests"].(map[string]any)
	if req["cpu"].(float64) != 0.5 {
		t.Errorf("cpu = %v, want 0.5 (the limit, not the request)", req["cpu"])
	}
	if req["memory"].(float64) != 256 {
		t.Errorf("memory = %v MiB, want 256 (the limit, not the request)", req["memory"])
	}
}

func TestBuildTemplateSurvivesPodWithoutResources(t *testing.T) {
	// A pod with no resources at all used to panic on an uninitialised map,
	// which took down the node rather than the pod.
	if _, err := BuildTemplate(pod(), TemplateOptions{}); err != nil {
		t.Fatalf("a pod without resources must still build: %v", err)
	}
}

func TestValidateRejectsUnrepresentableFields(t *testing.T) {
	cases := map[string]func(*corev1.Pod){
		"hostNetwork": func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		"hostPID":     func(p *corev1.Pod) { p.Spec.HostPID = true },
		"hostPath": func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{
				Name:         "h",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
			}}
		},
		"privileged": func(p *corev1.Pod) {
			yes := true
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes}
		},
		"probe with no handler": func(p *corev1.Pod) {
			p.Spec.Containers[0].LivenessProbe = &corev1.Probe{}
		},
		"probe aimed at another host": func(p *corev1.Pod) {
			p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{Host: "db.example.com", Port: intstr.FromInt32(80)},
				},
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildTemplate(pod(mutate), TemplateOptions{}); err == nil {
				t.Fatalf("%s was accepted; it must be refused with a message naming the field", name)
			}
		})
	}
}

func TestBuildTemplateCarriesProbes(t *testing.T) {
	// A workload whose health only it can answer — a DB cluster, a RAFT member
	// that may be on the wrong side of a split — is unusable without its own
	// probes, so they have to reach the runtime rather than be refused here.
	p := pod(func(p *corev1.Pod) {
		p.Spec.Containers[0].LivenessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"etcdctl", "endpoint", "health"}},
			},
			PeriodSeconds: 10,
		}
		p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)},
			},
			FailureThreshold: 3,
		}
	})
	raw, err := BuildTemplate(p, TemplateOptions{})
	if err != nil {
		t.Fatalf("probes were refused: %v", err)
	}
	c := decode(t, raw)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)

	live, ok := c["livenessProbe"].(map[string]any)
	if !ok {
		t.Fatal("livenessProbe did not reach the template")
	}
	cmd := live["exec"].(map[string]any)["command"].([]any)
	if len(cmd) != 3 || cmd[0] != "etcdctl" {
		t.Errorf("exec command was not carried through: %v", cmd)
	}
	if live["periodSeconds"].(float64) != 10 {
		t.Errorf("periodSeconds = %v, want 10", live["periodSeconds"])
	}

	// The httpGet became an exec against the container itself, since nothing
	// outside the container can reach its address.
	ready, ok := c["readinessProbe"].(map[string]any)
	if !ok {
		t.Fatal("readinessProbe did not reach the template")
	}
	if _, still := ready["httpGet"]; still {
		t.Error("httpGet reached the capsule unrewritten; it could never succeed there")
	}
	script := joinCommand(ready["exec"].(map[string]any)["command"].([]any))
	if !strings.Contains(script, "127.0.0.1:8080/ready") {
		t.Errorf("rewritten probe does not target the container itself: %s", script)
	}
	// Through the helper, not the image's own tools: a distroless image has
	// no shell to run them and no shell to say so either, so the container
	// would report unhealthy while answering perfectly well.
	if !strings.Contains(script, ProbeHelper) {
		t.Errorf("rewritten probe does not use the probe helper: %s", script)
	}
	if ready["failureThreshold"].(float64) != 3 {
		t.Errorf("failureThreshold = %v, want 3: timing must survive the rewrite",
			ready["failureThreshold"])
	}
}

func TestRewriteResolvesNamedPorts(t *testing.T) {
	// A probe naming a port resolves it from the container's declarations,
	// which is what the kubelet does; leaving the name in the command would
	// probe nothing.
	p := pod(func(p *corev1.Pod) {
		p.Spec.Containers[0].Ports = []corev1.ContainerPort{
			{Name: "metrics", ContainerPort: 9090},
		}
		p.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("metrics")},
			},
		}
	})
	raw, err := BuildTemplate(p, TemplateOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := decode(t, raw)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	cmd := c["readinessProbe"].(map[string]any)["exec"].(map[string]any)["command"].([]any)
	if script := joinCommand(cmd); !strings.Contains(script, "127.0.0.1:9090/healthz") {
		t.Errorf("named port was not resolved: %s", script)
	}
}

func TestRewriteRefusesUndeclaredNamedPort(t *testing.T) {
	p := pod(func(p *corev1.Pod) {
		p.Spec.Containers[0].LivenessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString("nope")},
			},
		}
	})
	if _, err := BuildTemplate(p, TemplateOptions{}); err == nil {
		t.Fatal("a probe naming an undeclared port was accepted; it would probe nothing")
	}
}

func TestExecProbesAreLeftAlone(t *testing.T) {
	// An exec probe needs no network and must reach the runtime verbatim —
	// this is the form a DB or RAFT member uses to answer for itself.
	p := pod(func(p *corev1.Pod) {
		p.Spec.Containers[0].LivenessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"etcdctl", "endpoint", "health"}},
			},
		}
	})
	raw, err := BuildTemplate(p, TemplateOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	c := decode(t, raw)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	cmd := c["livenessProbe"].(map[string]any)["exec"].(map[string]any)["command"].([]any)
	if len(cmd) != 3 || cmd[0] != "etcdctl" {
		t.Errorf("exec probe was altered: %v", cmd)
	}
}

func TestProbeOnTheContainerItselfIsAccepted(t *testing.T) {
	// An explicit localhost is the same target as the default and must not be
	// mistaken for a probe aimed elsewhere.
	for _, host := range []string{"", "localhost", "127.0.0.1"} {
		p := pod(func(p *corev1.Pod) {
			p.Spec.Containers[0].LivenessProbe = &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					TCPSocket: &corev1.TCPSocketAction{Host: host, Port: intstr.FromInt32(5432)},
				},
			}
		})
		if _, err := BuildTemplate(p, TemplateOptions{}); err != nil {
			t.Errorf("probe with host %q was refused: %v", host, err)
		}
	}
}

func TestPodKeyFromLabelsIgnoresForeignCapsules(t *testing.T) {
	// A capsule the tenant created through the Zun API directly carries no
	// ownership label; treating it as a pod would let cleanup delete it.
	if _, _, ok := PodKeyFromLabels(map[string]string{"app": "mine"}); ok {
		t.Error("a capsule without the ownership label was claimed as a managed pod")
	}
	ns, name, ok := PodKeyFromLabels(map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelNamespace: "111111-default",
		LabelPodName:   "web",
	})
	if !ok || ns != "111111-default" || name != "web" {
		t.Errorf("managed capsule not recognised: %q %q %v", ns, name, ok)
	}
}

// joinCommand renders an exec command for assertions, so a test says what it
// means regardless of how the arguments are split.
func joinCommand(cmd []any) string {
	parts := make([]string, 0, len(cmd))
	for _, a := range cmd {
		parts = append(parts, a.(string))
	}
	return strings.Join(parts, " ")
}

// TestEmptySecurityGroupsVanishFromTheTemplate documents the omitempty trap
// the deny-all anchor exists to neutralize: an explicitly empty list does not
// serialize as "securityGroups": [] — it serializes as NOTHING, and an absent
// field means the project default group downstream. If this test ever fails
// (because omitempty was removed and emptiness became explicit), the anchor's
// necessity should be re-argued with the whole chain verified — until then,
// netpol's TestFullyIsolatedPodNeverGetsAnEmptyGroupList keeps the empty list
// unreachable.
func TestEmptySecurityGroupsVanishFromTheTemplate(t *testing.T) {
	p := pod()
	tpl, err := BuildTemplate(p, TemplateOptions{SecurityGroups: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tpl), "securityGroups") {
		t.Fatal("an empty securityGroups now serializes explicitly — omitempty " +
			"was removed. Re-argue the deny-all anchor with the full chain " +
			"verified before relying on this.")
	}
}

// TestOwnerUIDGroupsReplicasNotPods pins the anti-affinity grouping key: two
// replicas of one workload share the label, a bare pod carries none. Grouping
// by pod name or pod UID makes every replica its own group and the weigher
// spreads nothing — silently.
func TestOwnerUIDGroupsReplicasNotPods(t *testing.T) {
	truthy := true
	owned := func(name string) *corev1.Pod {
		p := pod(func(p *corev1.Pod) { p.Name = name })
		p.OwnerReferences = []metav1.OwnerReference{{
			UID: "sts-1", Kind: "StatefulSet", Name: "keeper", Controller: &truthy}}
		return p
	}
	a, b := ownedLabels(owned("keeper-0")), ownedLabels(owned("keeper-1"))
	if a[LabelOwnerUID] == "" || a[LabelOwnerUID] != b[LabelOwnerUID] {
		t.Fatalf("replicas of one workload did not share an owner: %q vs %q",
			a[LabelOwnerUID], b[LabelOwnerUID])
	}
	if bare := ownedLabels(pod()); bare[LabelOwnerUID] != "" {
		t.Fatalf("a bare pod claimed an owner: %q", bare[LabelOwnerUID])
	}
}

// TestRuntimeClassNameReachesTheTemplate pins the tier-selection chain's first
// hop: a pod's runtimeClassName must land in the template verbatim, and its
// absence must leave the field out entirely — an empty string sent anyway
// would read as "the empty runtime" to a schema that validates the key.
func TestRuntimeClassNameReachesTheTemplate(t *testing.T) {
	fc := "kata-fc"
	pd := pod(func(p *corev1.Pod) { p.Spec.RuntimeClassName = &fc })
	raw, err := BuildTemplate(pd, TemplateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, raw)
	if got["runtime"] != "kata-fc" {
		t.Fatalf("runtimeClassName did not reach the template: %v — the tier "+
			"the tenant asked for silently fell back to the node default", got["runtime"])
	}

	raw, err = BuildTemplate(pod(), TemplateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got = decode(t, raw)
	if _, present := got["runtime"]; present {
		t.Fatal("an unset runtimeClassName sent a runtime key anyway")
	}
}
