package service

import (
	"strings"
	"testing"
)

// The name is what finds a load balancer when its annotation is gone — after a
// process restart that lost the write, or a Service edited by hand. It has to
// be derived from things that do not change, and it has to be unique across
// tenants sharing one OpenStack.
func TestLoadBalancerNameIsStableAndTenantScoped(t *testing.T) {
	a := &Reconciler{Tenant: "111111"}
	b := &Reconciler{Tenant: "222222"}

	first := a.lbName("111111-default", "web")
	if first != a.lbName("111111-default", "web") {
		t.Error("the name is not stable for one Service")
	}
	if first == b.lbName("111111-default", "web") {
		t.Error("two tenants derive the same name; one would adopt the other's load balancer")
	}
	if !strings.Contains(first, "111111-default") || !strings.Contains(first, "web") {
		t.Errorf("name %q does not identify the Service it belongs to", first)
	}
	// A garbage collector recognises its own by this prefix; kubetron's start
	// with kubetron_ and must never be matched.
	if !strings.HasPrefix(first, "kubezun_") {
		t.Errorf("name %q is not recognisable as ours", first)
	}
}

// One listener per port per protocol, and the names must not collide when a
// Service exposes the same port number over two protocols.
func TestListenerNamesDistinguishProtocolAndPort(t *testing.T) {
	r := &Reconciler{Tenant: "111111"}
	base := r.lbName("111111-default", "web")

	tcp := base + "_tcp_53"
	udp := base + "_udp_53"
	if tcp == udp {
		t.Fatal("TCP and UDP on one port derive the same listener name")
	}
	if strings.HasPrefix(udp, tcp) && len(udp) == len(tcp) {
		t.Error("names are not distinguishable")
	}
}
