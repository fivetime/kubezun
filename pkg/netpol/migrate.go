package netpol

import (
	"context"
	"fmt"
	"sort"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// DeviceOwnerCapsule is what Zun stamps on a port it made for a capsule
// (zun/common/consts.py:80). It is how a tenant's capsule ports are told from
// the tenant's other ports -- a load balancer VIP, a bare instance -- which
// must not be touched by any of this.
const DeviceOwnerCapsule = "compute:zun"

// Phase is one half of the conversion.
type Phase string

const (
	// PhaseAttach adds the policy groups to every capsule port, keeping
	// whatever was there. Every port is then strictly more permissive than
	// before, so nothing stops working part way through.
	PhaseAttach Phase = "attach"
	// PhaseDetach removes the project default group, which is what makes the
	// policy groups the only thing deciding reachability.
	PhaseDetach Phase = "detach"
)

// Convert moves a tenant onto policy-decided security groups, one phase at a
// time.
//
// ⚠️ Two phases, in this order, and not one pass per pod. A tenant's pods reach
// each other because all of them are in the project's default group, whose only
// ingress rule admits members of that same group. A pod moved out of it stops
// being accepted by every pod still in it -- and the refusal happens at the
// receiver, so the symptom appears on the wrong pod. Adding everywhere first
// keeps every port a superset of what it was; removing everywhere second flips
// the whole tenant at once, with no interval in which half the pods cannot
// reach the other half.
//
// Reads the ports from Neutron rather than from the pods this process knows.
// A pod this process has not seen -- another node's, one mid-creation -- is
// still a pod the tenant expects to keep working, and leaving its port behind
// is what makes a half-conversion.
func (n *Neutron) Convert(ctx context.Context, phase Phase, baseline []string, dryRun bool) (changed, total int, err error) {
	if len(baseline) == 0 {
		return 0, 0, fmt.Errorf("the baseline groups must be resolved first")
	}
	defaultID, err := n.defaultGroupID(ctx)
	if err != nil {
		return 0, 0, err
	}
	if phase == PhaseDetach && defaultID == "" {
		return 0, 0, fmt.Errorf("this project has no default security group, " +
			"so there is nothing to detach; run the attach phase first")
	}

	var list []ports.Port
	err = ports.List(n.Client, ports.ListOpts{
		DeviceOwner: DeviceOwnerCapsule,
	}).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		got, err := ports.ExtractPorts(page)
		if err != nil {
			return false, err
		}
		list = append(list, got...)
		return true, nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("listing this tenant's capsule ports: %w", err)
	}

	for _, port := range list {
		total++
		want := desiredGroups(phase, port.SecurityGroups, baseline, defaultID)
		if equalStrings(sorted(port.SecurityGroups), want) {
			continue
		}
		changed++
		if dryRun {
			continue
		}
		if _, err := ports.Update(ctx, n.Client, port.ID, ports.UpdateOpts{
			SecurityGroups: &want,
		}).Extract(); err != nil {
			return changed, total, fmt.Errorf("converting port %s: %w", port.ID, err)
		}
	}
	return changed, total, nil
}

// desiredGroups is what a port should carry after this phase.
func desiredGroups(phase Phase, current, baseline []string, defaultID string) []string {
	keep := map[string]struct{}{}
	for _, g := range current {
		keep[g] = struct{}{}
	}
	switch phase {
	case PhaseAttach:
		for _, g := range baseline {
			keep[g] = struct{}{}
		}
	case PhaseDetach:
		// ⚠️ Only the default group goes. A port may also carry groups the
		// tenant made for its own reasons, and this is not the place to
		// decide those are unwanted.
		delete(keep, defaultID)
		for _, g := range baseline {
			keep[g] = struct{}{}
		}
	}
	out := make([]string, 0, len(keep))
	for g := range keep {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

func (n *Neutron) defaultGroupID(ctx context.Context) (string, error) {
	id, err := n.findGroup(ctx, "default")
	if err != nil {
		return "", err
	}
	return id, nil
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
