package service

import "testing"

// The name a Service resolves under has to be the one Kubernetes clients
// already look for, or a chart's own configuration points at nothing.
func TestServiceDomainMatchesTheKubernetesShape(t *testing.T) {
	got := ServiceDomain("111111-default", "svc.cluster.local")
	if got != "111111-default.svc.cluster.local." {
		t.Errorf("domain = %q, want the namespace under the cluster domain, fully qualified", got)
	}

	// A trailing dot either way produces the same domain; DNS wants exactly one.
	if ServiceDomain("t1", "svc.cluster.local.") != ServiceDomain("t1", "svc.cluster.local") {
		t.Error("a trailing dot on the cluster domain changes the result")
	}
}

// The domain carries the namespace because a tenant has one network and may
// have several namespaces, so it cannot be a property of the network.
func TestServiceDomainSeparatesNamespaces(t *testing.T) {
	a := ServiceDomain("111111-default", "svc.cluster.local")
	b := ServiceDomain("111111-staging", "svc.cluster.local")
	if a == b {
		t.Error("two namespaces share a domain; their Services would collide by name")
	}
}
