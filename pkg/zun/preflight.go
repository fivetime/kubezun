package zun

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
)

// CheckAvailabilityZone confirms Zun offers the zone this node claims.
//
// A capsule asking for a zone that does not exist is refused by Zun's
// scheduler one pod at a time, as "no valid host" -- an answer that reads like
// a full cluster rather than a misconfigured node, and that arrives only once
// a tenant tries to run something. Asking once at startup turns it into a
// message naming the zone and listing the ones that exist.
//
// This is as far as verification goes from here. Whether the deployment has
// the hardware a node claims (its architecture) cannot be checked at all:
// Zun's hosts API is admin-only and this process holds a tenant credential, on
// purpose. ValidateArchitecture catches a typo; a well-spelled architecture
// nobody runs is provisioning's to get right.
func (c *Client) CheckAvailabilityZone(ctx context.Context, zone string) error {
	if zone == "" {
		return nil
	}
	zones, err := c.availabilityZones(ctx)
	if err != nil || len(zones) == 0 {
		// Not being able to ask, or not recognising the answer, is not
		// evidence the zone is missing. Refusing on it would take a working
		// node down over an API that has nothing to do with running pods --
		// which this check did on its first run, having read the wrong field
		// and got a list of names that were all empty.
		return nil
	}
	return refuseUnknownZone(zone, zones)
}

// refuseUnknownZone is the decision on its own, so it can be tested without a
// server: an empty zone or an unreadable list is always accepted.
func refuseUnknownZone(zone string, zones []string) error {
	if zone == "" || len(zones) == 0 {
		return nil
	}
	for _, z := range zones {
		if z == zone {
			return nil
		}
	}
	sort.Strings(zones)
	return fmt.Errorf("this deployment has no availability zone %q; it offers %s",
		zone, strings.Join(zones, ", "))
}

// availabilityZones returns the zone names Zun reports, dropping any entry it
// could not read a name from.
//
// Both spellings are decoded because the field is "availability_zone" in the
// response and "name" in most other OpenStack collections. Reading the wrong
// one yields a list of empty strings — a non-empty list matching nothing,
// which is how this check first refused to start a node whose zone was
// perfectly real. Entries without a name are dropped rather than kept as "",
// so a shape that is not understood ends as an empty list, which the caller
// treats as "could not ask".
func (c *Client) availabilityZones(ctx context.Context) ([]string, error) {
	var body struct {
		AvailabilityZones []struct {
			Zone string `json:"availability_zone"`
			Name string `json:"name"`
		} `json:"availability_zones"`
	}
	_, err := c.sc.Get(ctx, c.sc.ServiceURL("availability_zones"), &body,
		&gophercloud.RequestOpts{OkCodes: []int{200}})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(body.AvailabilityZones))
	for _, z := range body.AvailabilityZones {
		switch {
		case z.Zone != "":
			out = append(out, z.Zone)
		case z.Name != "":
			out = append(out, z.Name)
		}
	}
	return out, nil
}
