package provider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func podWithDNS(namespace string, cfg *corev1.PodDNSConfig) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "p"},
		Spec:       corev1.PodSpec{DNSConfig: cfg},
	}
}

// The gateway writes the tenant's own view into the pod: the namespace their
// manifests name, and their own resolver. Composing it here would produce
// "<tenant>-default.svc.cluster.local", which this cluster stores and nobody
// asks for.
func TestDNSConfigPrefersWhatThePodCarries(t *testing.T) {
	pod := podWithDNS("111111-default", &corev1.PodDNSConfig{
		Searches:    []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"},
		Nameservers: []string{"254.51.215.104"},
	})

	searches, nameservers := dnsConfigFor(pod, "svc.cluster.local")
	if len(searches) == 0 || searches[0] != "default.svc.cluster.local" {
		t.Errorf("searches = %v, want the tenant's own namespace first", searches)
	}
	for _, s := range searches {
		if s == "111111-default.svc.cluster.local" {
			t.Error("composed the stored namespace over what the pod carried")
		}
	}
	if len(nameservers) != 1 || nameservers[0] != "254.51.215.104" {
		t.Errorf("nameservers = %v, want the tenant's resolver", nameservers)
	}
}

// The gateway leaves the config empty while the tenant's resolver is not yet
// serving. A pod created in that window with no search list resolves nothing by
// short name, so a composed list is better than none.
func TestDNSConfigFallsBackWhenThePodCarriesNone(t *testing.T) {
	searches, nameservers := dnsConfigFor(podWithDNS("111111-default", nil), "svc.cluster.local")
	if len(searches) != 3 || searches[0] != "111111-default.svc.cluster.local" {
		t.Errorf("searches = %v, want a composed list", searches)
	}
	// No resolver is named, so the subnet's stands — the capsule still resolves
	// public names rather than nothing at all.
	if nameservers != nil {
		t.Errorf("nameservers = %v, want none so the subnet's is used", nameservers)
	}

	empty := podWithDNS("111111-default", &corev1.PodDNSConfig{})
	if s, _ := dnsConfigFor(empty, "svc.cluster.local"); len(s) != 3 {
		t.Errorf("an empty config should fall back too, got %v", s)
	}
}

// The fallback is the shape a kubelet composes, and the order matters: a pod's
// own namespace has to be searched first, or a Service of the same name in
// another namespace answers instead.
func TestComposedFallbackMatchesTheKubeletShape(t *testing.T) {
	got := composeSearches("111111-default", "svc.cluster.local")
	want := []string{"111111-default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
	if composeSearches("t1", "") != nil {
		t.Error("no cluster domain should produce no list rather than a broken one")
	}
}
