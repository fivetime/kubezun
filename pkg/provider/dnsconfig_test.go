package provider

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fivetime/kubezun/pkg/service"
)

// fakeObjects answers Service reads and nothing else; the DNS path touches no
// ConfigMaps or Secrets.
type fakeObjects struct {
	svc *corev1.Service
	err error
}

func (f fakeObjects) ConfigMap(context.Context, string, string) (*corev1.ConfigMap, error) {
	return nil, fmt.Errorf("not used")
}
func (f fakeObjects) Secret(context.Context, string, string) (*corev1.Secret, error) {
	return nil, fmt.Errorf("not used")
}
func (f fakeObjects) Service(_ context.Context, namespace, name string) (*corev1.Service, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.svc == nil || f.svc.Namespace != namespace || f.svc.Name != name {
		return nil, fmt.Errorf("no Service %s/%s", namespace, name)
	}
	return f.svc, nil
}

func dnsService(namespace, name, clusterIP, address string) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.ServiceSpec{ClusterIP: clusterIP},
	}
	if address != "" {
		svc.Annotations = map[string]string{service.AddressAnnotation: address}
	}
	return svc
}

func testProvider(objects ObjectReader) *Provider {
	return &Provider{
		cfg: Config{
			ClusterDomain: "svc.cluster.local",
			Tenant:        "111111",
			DNSService:    "kube-system/kube-dns",
		},
		objects: objects,
	}
}

func pod(namespace string, policy corev1.DNSPolicy, cfg *corev1.PodDNSConfig) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "p"},
		Spec:       corev1.PodSpec{DNSPolicy: policy, DNSConfig: cfg},
	}
}

// An ordinary pod carries no dnsConfig at all -- dnsPolicy defaults to
// ClusterFirst and a kubelet fills the rest in. There is no kubelet here, and
// when this filled in nothing the capsule inherited the Neutron subnet's
// resolver: a public one, which answers NXDOMAIN for every in-cluster name
// while the tenant's own CoreDNS sits there serving correct records that
// nothing asks it for.
func TestClusterFirstGetsTheTenantResolverAndTenantNamespace(t *testing.T) {
	p := testProvider(fakeObjects{
		svc: dnsService("111111-kube-system", "kube-dns", "254.51.215.104", "192.168.200.244"),
	})

	searches, nameservers := p.dnsConfigFor(context.Background(), pod("111111-default", "", nil))

	if len(nameservers) != 1 || nameservers[0] != "192.168.200.244" {
		t.Errorf("nameservers = %v, want the load balancer address", nameservers)
	}
	// The clusterIP is the gateway's fiction; nothing on the tenant network
	// routes it, so a capsule given it times out on every lookup.
	for _, ns := range nameservers {
		if ns == "254.51.215.104" {
			t.Error("handed out the clusterIP, which does not answer from a capsule")
		}
	}
	want := []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	if len(searches) != len(want) {
		t.Fatalf("searches = %v, want %v", searches, want)
	}
	for i := range want {
		if searches[i] != want[i] {
			t.Errorf("search %d = %q, want %q", i, searches[i], want[i])
		}
	}
}

// The tenant prefix has to come off. Their resolver watches through the gateway
// and serves "default", so searching "111111-default" finds nothing.
func TestSearchesUseTheNamespaceTheTenantWrote(t *testing.T) {
	p := testProvider(fakeObjects{svc: dnsService("111111-kube-system", "kube-dns", "", "10.0.0.1")})
	searches, _ := p.dnsConfigFor(context.Background(), pod("111111-kube-system", corev1.DNSClusterFirst, nil))
	if searches[0] != "kube-system.svc.cluster.local" {
		t.Errorf("searches[0] = %q, want the unprefixed namespace", searches[0])
	}

	// A namespace that merely starts with the same digits is not prefixed.
	unprefixed := testProvider(fakeObjects{svc: dnsService("111111-kube-system", "kube-dns", "", "10.0.0.1")})
	unprefixed.cfg.Tenant = "222222"
	searches, _ = unprefixed.dnsConfigFor(context.Background(), pod("111111-default", corev1.DNSClusterFirst, nil))
	if searches[0] != "111111-default.svc.cluster.local" {
		t.Errorf("searches[0] = %q, want the namespace untouched", searches[0])
	}
}

func TestPodDNSConfigAddsToAndOverridesTheDefaults(t *testing.T) {
	p := testProvider(fakeObjects{svc: dnsService("111111-kube-system", "kube-dns", "", "192.168.200.244")})

	searches, nameservers := p.dnsConfigFor(context.Background(),
		pod("111111-default", corev1.DNSClusterFirst, &corev1.PodDNSConfig{
			Searches:    []string{"extra.example.com"},
			Nameservers: []string{"10.9.9.9"},
		}))

	if len(searches) != 4 || searches[3] != "extra.example.com" {
		t.Errorf("searches = %v, want the composed list plus the pod's", searches)
	}
	if len(nameservers) != 1 || nameservers[0] != "10.9.9.9" {
		t.Errorf("nameservers = %v, want the pod's own to win", nameservers)
	}
}

func TestExplicitPoliciesAreHonoured(t *testing.T) {
	p := testProvider(fakeObjects{svc: dnsService("111111-kube-system", "kube-dns", "", "192.168.200.244")})

	// None: exactly what the pod asked for, nothing composed.
	searches, nameservers := p.dnsConfigFor(context.Background(),
		pod("111111-default", corev1.DNSNone, &corev1.PodDNSConfig{
			Searches: []string{"only.example.com"}, Nameservers: []string{"10.1.1.1"},
		}))
	if len(searches) != 1 || searches[0] != "only.example.com" {
		t.Errorf("None: searches = %v", searches)
	}
	if len(nameservers) != 1 || nameservers[0] != "10.1.1.1" {
		t.Errorf("None: nameservers = %v", nameservers)
	}

	// Default: the infrastructure's resolver, which here is the subnet's.
	searches, nameservers = p.dnsConfigFor(context.Background(),
		pod("111111-default", corev1.DNSDefault, nil))
	if searches != nil || nameservers != nil {
		t.Errorf("Default: got %v / %v, want neither", searches, nameservers)
	}
}

// A pod created before the resolver's load balancer exists still starts. It
// gets the subnet's resolver, and its controller replaces it soon enough.
func TestAMissingResolverAddressDoesNotBlockThePod(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objects ObjectReader
	}{
		{"service not there yet", fakeObjects{err: fmt.Errorf("not found")}},
		{"no address on it yet", fakeObjects{svc: dnsService("111111-kube-system", "kube-dns", "254.51.215.104", "")}},
	} {
		p := testProvider(tc.objects)
		searches, nameservers := p.dnsConfigFor(context.Background(), pod("111111-default", "", nil))
		if nameservers != nil {
			t.Errorf("%s: nameservers = %v, want none", tc.name, nameservers)
		}
		if len(searches) != 3 {
			t.Errorf("%s: searches = %v, want the composed list regardless", tc.name, searches)
		}
	}
}

// The fallback is the shape a kubelet composes, and the order matters: a pod's
// own namespace has to be searched first, or a Service of the same name in
// another namespace answers instead.
func TestComposedFallbackMatchesTheKubeletShape(t *testing.T) {
	got := composeSearches("default", "svc.cluster.local")
	want := []string{"default.svc.cluster.local", "svc.cluster.local", "cluster.local"}
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
