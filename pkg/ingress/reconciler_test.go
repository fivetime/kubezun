package ingress

import (
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/l7policies"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The two name schemes share a prefix and are divided by segment shape. Getting
// this wrong is not cosmetic: each sweep deletes what it parses as its own and
// orphaned, so a parser that reads the other kind's names deletes the other
// kind's live load balancers.
func TestNameSchemesDoNotCollide(t *testing.T) {
	r := &Reconciler{Tenant: "111111"}

	name := r.lbName("111111-default", "web")
	if name != "kubezun_111111_ing_111111-default_web" {
		t.Fatalf("unexpected ingress LB name %q", name)
	}

	ns, ing, ok := ParseLBName("111111", name)
	if !ok || ns != "111111-default" || ing != "web" {
		t.Fatalf("ParseLBName(%q) = %q,%q,%v", name, ns, ing, ok)
	}

	// A Service load balancer's name must not parse as an Ingress one.
	if _, _, ok := ParseLBName("111111", "kubezun_111111_111111-default_web"); ok {
		t.Fatal("a Service LB name parsed as an Ingress LB; the ingress sweep would delete it")
	}
	// Another tenant's never parses.
	if _, _, ok := ParseLBName("111111", "kubezun_222222_ing_222222-default_web"); ok {
		t.Fatal("another tenant's LB parsed as ours")
	}
}

func TestOursHonoursClassAndLegacyAnnotation(t *testing.T) {
	r := &Reconciler{ClassName: "knaas", Tenant: "111111"}
	cls := "knaas"
	prefixed := "111111-knaas"
	foreignPrefixed := "222222-knaas"
	other := "nginx"

	cases := []struct {
		name string
		ing  networkingv1.Ingress
		want bool
	}{
		{"spec class matches", networkingv1.Ingress{Spec: networkingv1.IngressSpec{IngressClassName: &cls}}, true},
		// The gateway prefixes tenant-owned cluster-scoped names, so what the
		// tenant wrote as "knaas" reads as "111111-knaas" here. Missing this
		// sent every tenant Ingress down the teardown path.
		{"tenant-prefixed class", networkingv1.Ingress{Spec: networkingv1.IngressSpec{IngressClassName: &prefixed}}, true},
		{"another tenant's prefix", networkingv1.Ingress{Spec: networkingv1.IngressSpec{IngressClassName: &foreignPrefixed}}, false},
		{"spec class other", networkingv1.Ingress{Spec: networkingv1.IngressSpec{IngressClassName: &other}}, false},
		{"legacy annotation", networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{LegacyClassAnnotation: "knaas"}}}, true},
		{"no class at all", networkingv1.Ingress{}, false},
	}
	for _, tc := range cases {
		if got := r.Ours(&tc.ing); got != tc.want {
			t.Errorf("%s: Ours = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Longest path first, whatever order the manifest used: Octavia evaluates
// policies by position, and "/" placed first would swallow every request.
func TestPoliciesOrderMostSpecificFirst(t *testing.T) {
	prefix := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{
			Host: "app.example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
				Paths: []networkingv1.HTTPIngressPath{
					{Path: "/", PathType: &prefix, Backend: backend("web", 80)},
					{Path: "/api/v2", PathType: &prefix, Backend: backend("api", 8080)},
					{Path: "/api", PathType: &prefix, Backend: backend("api", 8080)},
				},
			}},
		}}},
	}
	pools := map[string]string{
		"lb_web_80":   "pool-web",
		"lb_api_8080": "pool-api",
	}
	got := buildDesiredPolicies(ing, "lb", pools)
	if len(got) != 3 {
		t.Fatalf("expected 3 policies, got %d", len(got))
	}
	if got[0].Path != "/api/v2" || got[1].Path != "/api" || got[2].Path != "/" {
		t.Errorf("policies out of order: %q %q %q — a request to /api/v2 would hit the wrong pool",
			got[0].Path, got[1].Path, got[2].Path)
	}
}

// Every policy must carry at least one rule. Octavia matches a policy by its
// rules, so a policy with none matches nothing -- "/" was skipped on the theory
// that it needs no rule, and every request to it came back 503 while the longer
// paths of the same Ingress served.
func TestEveryPolicyHasAMatchingRule(t *testing.T) {
	exact := networkingv1.PathTypeExact
	prefix := networkingv1.PathTypePrefix
	cases := []desiredPolicy{
		{Path: "/", PathType: prefix},
		{Path: "/api", PathType: prefix},
		{Path: "/health", PathType: exact},
		{Host: "app.example.com", Path: "/", PathType: prefix},
	}
	for _, d := range cases {
		rules := rulesFor(d)
		if len(rules) == 0 {
			t.Errorf("host=%q path=%q produced no rules: the policy would never match", d.Host, d.Path)
			continue
		}
		var hasPath bool
		for _, r := range rules {
			if r.RuleType == l7policies.TypePath {
				hasPath = true
				want := l7policies.CompareTypeStartWith
				if d.PathType == exact {
					want = l7policies.CompareTypeEqual
				}
				if r.CompareType != want {
					t.Errorf("path %q: compare type %v, want %v", d.Path, r.CompareType, want)
				}
				if r.Value != d.Path {
					t.Errorf("path %q: rule value %q", d.Path, r.Value)
				}
			}
		}
		if !hasPath {
			t.Errorf("host=%q path=%q produced no path rule", d.Host, d.Path)
		}
	}
}

// A backend referenced by two paths maps to one pool; the default backend
// joins the same set.
func TestCollectBackendsDeduplicates(t *testing.T) {
	prefix := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		Spec: networkingv1.IngressSpec{
			DefaultBackend: ptr(backend("web", 80)),
			Rules: []networkingv1.IngressRule{{
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{Path: "/", PathType: &prefix, Backend: backend("web", 80)},
						{Path: "/api", PathType: &prefix, Backend: backend("api", 8080)},
					},
				}},
			}},
		},
	}
	def, all := collectBackends(ing)
	if def == nil || def.Service != "web" {
		t.Fatalf("default backend lost: %+v", def)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 distinct backends, got %d: %+v", len(all), all)
	}
}

// The exposure default is VIP-only: a public address is billed and reachable
// from the world, so it must be an explicit ask.
func TestInternalByDefault(t *testing.T) {
	if !internal(&networkingv1.Ingress{}) {
		t.Fatal("an Ingress with no annotations would get a public address")
	}
	pub := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{InternalAnnotation: "false"}}}
	if internal(pub) {
		t.Fatal("internal=false was not honoured")
	}
}

// Ownership of a floating IP lives in its description, so teardown can decide
// delete-versus-detach without the Ingress object.
func TestFIPOwnership(t *testing.T) {
	lb := "kubezun_111111_ing_ns_web"
	if !ownedBy(fipDescription(lb, false), lb) || !ownedBy(fipDescription(lb, true), lb) {
		t.Fatal("our own descriptions do not read as ours")
	}
	if ownedBy("tenant's own FIP", lb) || ownedBy("kubezun_111111_ing_ns_other", lb) {
		t.Fatal("somebody else's floating IP read as ours; teardown would delete it")
	}
}

func backend(svc string, port int32) networkingv1.IngressBackend {
	return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
		Name: svc, Port: networkingv1.ServiceBackendPort{Number: port},
	}}
}

func ptr[T any](v T) *T { return &v }

// List(nil) panics inside the lister — caught live on first deployment: every
// EndpointSlice event crashed the process. The selector must always be
// labels.Everything(); this pins the fan-out path with a real lister.

// The provider is per-Ingress, so one tenant can run an Ingress on a provider
// that does what they need and another on the cheaper one.
func TestProviderAnnotationOverridesTheDefault(t *testing.T) {
	r := &Reconciler{Provider: "incus"}

	got, err := r.resolveProvider(&networkingv1.Ingress{})
	if err != nil || got != "incus" {
		t.Errorf("unset = %q, %v; want the deployment default", got, err)
	}

	got, err = r.resolveProvider(&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{ProviderAnnotation: "amphora"}}})
	if err != nil || got != "amphora" {
		t.Errorf("annotated = %q, %v; want what the Ingress asked for", got, err)
	}

	// Whitespace is a typo, not a provider name.
	got, err = r.resolveProvider(&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{ProviderAnnotation: "  amphora  "}}})
	if err != nil || got != "amphora" {
		t.Errorf("padded = %q, %v", got, err)
	}
}

// ovn serves every Service here, so it is the name a tenant reaches for first —
// and it is the one provider that cannot serve an Ingress at all.
func TestOVNIsRefusedForIngress(t *testing.T) {
	r := &Reconciler{Provider: "incus"}
	_, err := r.resolveProvider(&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{ProviderAnnotation: "ovn"}}})
	if err == nil {
		t.Fatal("ovn was accepted; it would build a load balancer that refuses every listener")
	}
	if !strings.Contains(err.Error(), "layer 4") {
		t.Errorf("the error should say why, got: %v", err)
	}
}

// Octavia cannot move a load balancer between providers. Reconciling on anyway
// serves the Ingress from a provider its spec no longer names, with everything
// reporting healthy; recreating it silently drops a published address.
func TestAChangedProviderIsRefusedNotAppliedSilently(t *testing.T) {
	live := &loadbalancers.LoadBalancer{ID: "lb-1", Provider: "incus"}

	if err := providerMatches(live, "incus"); err != nil {
		t.Errorf("same provider should reconcile: %v", err)
	}
	err := providerMatches(live, "amphora")
	if err == nil {
		t.Fatal("a changed provider was accepted")
	}
	if !strings.Contains(err.Error(), "amphora") || !strings.Contains(err.Error(), "incus") {
		t.Errorf("the error should name both providers, got: %v", err)
	}

	// Older Octavia omits the provider. Refusing on missing information would
	// strand every Ingress on such a deployment.
	if err := providerMatches(&loadbalancers.LoadBalancer{ID: "lb-2"}, "amphora"); err != nil {
		t.Errorf("an unreported provider should not block a reconcile: %v", err)
	}
}
