package netpol

import (
	"reflect"
	"testing"
)

// TestAttachOnlyEverAdds is the property the first phase rests on. While the
// conversion is running, every port must be at least as permissive as it was:
// a port that loses anything mid-conversion is a pod that stops answering, and
// the pod that notices is the one at the other end.
func TestAttachOnlyEverAdds(t *testing.T) {
	baseline := []string{"sg-deny", "sg-in", "sg-eg"}
	current := []string{"sg-default", "sg-tenant-own"}

	got := desiredGroups(PhaseAttach, current, baseline, "sg-default")
	for _, had := range current {
		if !contains(got, had) {
			t.Errorf("attach removed %s: %v", had, got)
		}
	}
	for _, want := range baseline {
		if !contains(got, want) {
			t.Errorf("attach did not add %s: %v", want, got)
		}
	}
}

// TestDetachTakesTheDefaultAndNothingElse keeps the second phase from tidying
// away groups the tenant made for its own reasons, which is not this
// operation's business.
func TestDetachTakesTheDefaultAndNothingElse(t *testing.T) {
	baseline := []string{"sg-deny", "sg-in", "sg-eg"}
	got := desiredGroups(PhaseDetach,
		[]string{"sg-default", "sg-tenant-own", "sg-deny", "sg-in", "sg-eg"},
		baseline, "sg-default")

	want := []string{"sg-deny", "sg-eg", "sg-in", "sg-tenant-own"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detach = %v, want %v", got, want)
	}
}

// TestConversionIsIdempotent matters because it will be re-run: an operator
// who is unsure whether a phase finished must be able to run it again without
// a second pass doing anything.
func TestConversionIsIdempotent(t *testing.T) {
	baseline := []string{"sg-deny", "sg-in", "sg-eg"}
	once := desiredGroups(PhaseDetach, []string{"sg-default"}, baseline, "sg-default")
	twice := desiredGroups(PhaseDetach, once, baseline, "sg-default")
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("a second pass changed things: %v then %v", once, twice)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
