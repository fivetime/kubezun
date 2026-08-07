package service

import "testing"

// The sweep decides what to delete from a name, so the parse has to be exact:
// a false positive takes a live Service off the air.
func TestParseLBNameRecoversTheService(t *testing.T) {
	r := &Reconciler{Tenant: "111111"}

	ns, svc, ok := r.parseLBName(r.lbName("111111-default", "web"))
	if !ok || ns != "111111-default" || svc != "web" {
		t.Errorf("parse of our own name gave %q %q %v", ns, svc, ok)
	}
}

func TestParseLBNameRefusesEverythingElse(t *testing.T) {
	r := &Reconciler{Tenant: "111111"}

	for _, name := range []string{
		// Another tenant's: deleting one would take down someone else entirely.
		"kubezun_222222_222222-default_web",
		// kubetron's, which shares the OpenStack project in a mixed deployment.
		"kubetron_111111-default_web",
		// An operator's own, made by hand.
		"t1-lb",
		"",
		// Ours by prefix but malformed, so the Service cannot be identified.
		"kubezun_111111_",
		"kubezun_111111_nonamespace",
	} {
		if _, _, ok := r.parseLBName(name); ok {
			t.Errorf("parseLBName(%q) claimed the load balancer as ours", name)
		}
	}
}
