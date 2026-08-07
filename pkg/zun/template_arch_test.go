package zun

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateArchitecture(t *testing.T) {
	for _, ok := range []string{"", "amd64", "arm64", "s390x"} {
		if err := ValidateArchitecture(ok); err != nil {
			t.Errorf("ValidateArchitecture(%q) = %v, want nil", ok, err)
		}
	}
	// x86_64 is the Linux spelling; the node label and this flag use the
	// Kubernetes one, and accepting both here would let a node register under
	// a label no scheduler predicate matches.
	for _, bad := range []string{"x86_64", "aarch64", "x86", "AMD64"} {
		if err := ValidateArchitecture(bad); err == nil {
			t.Errorf("ValidateArchitecture(%q) = nil, want error", bad)
		}
	}
}

// A capsule must carry the architecture down to Zun. Without it Placement has
// no trait to filter on and the capsule lands wherever there is room, failing
// only when the container tries to execute an image built for another machine.
func TestBuildTemplateCarriesArchitecture(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: "p", UID: "u1"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "nginx"}},
		},
	}
	tpl, err := BuildTemplate(pod, TemplateOptions{Architecture: "arm64"})
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(tpl, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["architecture"] != "arm64" {
		t.Errorf("architecture = %v, want arm64", got["architecture"])
	}

	// Omitted, not empty: an older Zun rejects an unknown template field, so a
	// node that states no architecture must send no field at all.
	tpl2, err := BuildTemplate(pod, TemplateOptions{})
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	var got2 map[string]any
	_ = json.Unmarshal(tpl2, &got2)
	if _, present := got2["architecture"]; present {
		t.Errorf("architecture present when unset: %s", tpl2)
	}
}

// The node name has to reach the capsule: the orphan sweep uses it to tell its
// own capsules from a sibling node's, and an unstamped capsule is never
// cleaned up.
func TestBuildTemplateStampsNodeName(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "t1", Name: "p", UID: "u1"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
	}
	tpl, err := BuildTemplate(pod, TemplateOptions{NodeName: "t1-node-arm64"})
	if err != nil {
		t.Fatalf("BuildTemplate: %v", err)
	}
	var got struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(tpl, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Metadata.Labels[LabelNodeName] != "t1-node-arm64" {
		t.Errorf("node label = %q, want t1-node-arm64", got.Metadata.Labels[LabelNodeName])
	}

	// Absent rather than empty when unknown: the sweep reads an empty value as
	// a node named "", which would make it claim capsules it does not own.
	tpl2, _ := BuildTemplate(pod, TemplateOptions{})
	var bare struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(tpl2, &bare)
	if v, present := bare.Metadata.Labels[LabelNodeName]; present {
		t.Errorf("node label present with value %q when no node name was given", v)
	}
}
