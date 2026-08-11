package netpol

import "testing"

// TestPolicyOfReadsBackWhatEnsureWrote keeps the sweep's only means of
// identification working. The group name is a hash, so the description is the
// only thing that says whose it is -- and a sweep that cannot tell is a sweep
// that must either leak forever or delete another process's groups.
func TestPolicyOfReadsBackWhatEnsureWrote(t *testing.T) {
	written := "kubezun: what NetworkPolicy prod/web-deny allows"
	if got := policyOf(written); got != "prod/web-deny" {
		t.Errorf("policyOf(%q) = %q", written, got)
	}
	// Anything not written by this must be left alone, not misparsed into a
	// name that happens to look sweepable.
	for _, other := range []string{
		"", "default", "kubezun: allows nothing",
		"some other tool's group", "kubezun: what NetworkPolicy prod/web",
	} {
		if got := policyOf(other); got != "" {
			t.Errorf("policyOf(%q) claimed %q", other, got)
		}
	}
}
