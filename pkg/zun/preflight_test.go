package zun

import "testing"

// The response names the field "availability_zone", not "name". Reading the
// wrong one gives a list of empty strings: non-empty, matching nothing, so
// every zone looks missing. That refused to start a node whose zone was real.
func TestZoneCheckOnlyRefusesOnNamesItActuallyRead(t *testing.T) {
	cases := []struct {
		name   string
		zones  []string
		want   string
		refuse bool
	}{
		{"zone is offered", []string{"nova", "az2"}, "nova", false},
		{"zone is not offered", []string{"az2", "az3"}, "nova", true},
		{"nothing was readable", nil, "nova", false},
		{"no zone was asked for", []string{"az2"}, "", false},
	}
	for _, tc := range cases {
		err := refuseUnknownZone(tc.want, tc.zones)
		if tc.refuse && err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
		if !tc.refuse && err != nil {
			t.Errorf("%s: refused with %v", tc.name, err)
		}
	}
}
