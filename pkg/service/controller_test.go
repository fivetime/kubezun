package service

import "testing"

// The Service informer spans the cluster, because a tenant's namespaces are not
// known until they are watched. So the namespace check is the only thing
// standing between this controller and every Service in the cluster — and what
// it builds is a load balancer in the tenant's own OpenStack project, against
// the tenant's quota. Without it, 19 appeared: the platform's Cilium and Kyverno
// services, the cluster's own kubernetes Service, and another tenant's kube-dns.
func TestServesRefusesWhenUnset(t *testing.T) {
	c := &Controller{reconciler: &Reconciler{}}
	if c.serves("111111-default") {
		t.Fatal("served a namespace with no check configured")
	}
}

func TestServesAsksTheCheck(t *testing.T) {
	served := map[string]bool{"111111-default": true, "111111-kube-system": true}
	c := &Controller{reconciler: &Reconciler{
		ServesNamespace: func(ns string) bool { return served[ns] },
	}}

	for _, ns := range []string{"111111-default", "111111-kube-system"} {
		if !c.serves(ns) {
			t.Fatalf("refused %s, which this tenant owns", ns)
		}
	}
	// The three that actually appeared as load balancers.
	for _, ns := range []string{"kube-system", "kyverno", "222222-kube-system", "default"} {
		if c.serves(ns) {
			t.Fatalf("accepted %s, which belongs to somebody else", ns)
		}
	}
}
