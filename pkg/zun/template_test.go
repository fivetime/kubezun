package zun

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		"livenessProbe": func(p *corev1.Pod) {
			p.Spec.Containers[0].LivenessProbe = &corev1.Probe{}
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
