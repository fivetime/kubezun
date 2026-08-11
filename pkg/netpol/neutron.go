package netpol

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/addressgroups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

// The two groups that carry the baseline. They exist for the life of the
// tenant and never change, which is deliberate: creating or deleting a
// security group OBJECT makes ovn-northd rebuild every port group in the
// cloud -- every other tenant's, nova's, Octavia's (DESIGN §7.7.3). Adding
// rules to an existing group does not.
const (
	AllowIngress = "knp-allow-ingress"
	AllowEgress  = "knp-allow-egress"

	// DenyAll carries no rules at all, and exists so that a port's group list
	// is never empty.
	//
	// ⚠️ Empty and absent are opposite meanings and one API cannot tell them
	// apart. Neutron injects the project's default group -- which is
	// permissive -- when a port names no groups, so a pod isolated in both
	// directions, whose correct group list is empty, would come up reachable
	// by everything. Naming a group that allows nothing says deny-all in a way
	// that survives the round trip.
	DenyAll = "knp-deny-all"
)

// managedBy marks what this controller owns, so a sweep can tell our objects
// from a tenant's own and from another tool's.
const managedBy = "kubezun-networkpolicy"

// Neutron is the part of the network API this package uses.
type Neutron struct {
	Client *gophercloud.ServiceClient
}

// EnsureBaseline makes sure the two allow-everything groups exist, and returns
// their ids.
//
// ⚠️ Two groups, not one. A Neutron security group has no direction of its own
// -- direction lives on the rule -- while Kubernetes isolates a pod per
// direction. One combined group, detached from a pod whose policy names only
// Ingress, would take that pod's egress with it.
func (n *Neutron) EnsureBaseline(ctx context.Context) (ingressID, egressID string, err error) {
	if _, err := n.EnsureDenyAll(ctx); err != nil {
		return "", "", err
	}
	ingressID, err = n.ensureGroup(ctx, AllowIngress,
		"kubezun: a pod no policy has isolated for ingress")
	if err != nil {
		return "", "", err
	}
	egressID, err = n.ensureGroup(ctx, AllowEgress,
		"kubezun: a pod no policy has isolated for egress")
	if err != nil {
		return "", "", err
	}
	if err := n.ensureBaselineRules(ctx, ingressID, "ingress"); err != nil {
		return "", "", err
	}
	if err := n.ensureBaselineRules(ctx, egressID, "egress"); err != nil {
		return "", "", err
	}
	return ingressID, egressID, nil
}

// ensureBaselineRules opens one direction completely, for both address
// families, and closes the other.
//
// The opening is needed because Neutron's baseline is the opposite of
// Kubernetes': a port carrying security groups is denied whatever no rule
// allows, while a pod no policy selects must be able to talk. So "no policy
// applies" has to be stated rather than left unsaid.
//
// ⚠️ The closing is needed because Neutron adds two egress allow-all rules to
// every security group it creates, which nobody asked for and which the API
// does not mention at creation time. Left in place they make the ingress group
// allow all egress as well -- so detaching the egress group from an isolated
// pod changes nothing, and egress policy is enforced in appearance only. That
// is the failure this whole package exists to remove, so a group here carries
// rules for its own direction and nothing else.
func (n *Neutron) ensureBaselineRules(ctx context.Context, groupID, direction string) error {
	want := []rules.CreateOpts{
		{SecGroupID: groupID, Direction: rules.RuleDirection(direction),
			EtherType: rules.EtherType4, RemoteIPPrefix: "0.0.0.0/0"},
		{SecGroupID: groupID, Direction: rules.RuleDirection(direction),
			EtherType: rules.EtherType6, RemoteIPPrefix: "::/0"},
	}
	have, err := n.rulesOf(ctx, groupID)
	if err != nil {
		return err
	}
	for _, w := range want {
		if hasRule(have, w) {
			continue
		}
		if _, err := rules.Create(ctx, n.Client, w).Extract(); err != nil {
			// Another process of the same tenant may have written it between
			// the read and the write, and Neutron reads a rule with no remote
			// prefix as the same rule as one naming the whole address space.
			// Either way the state we wanted is the state that exists.
			if !isConflict(err) {
				return fmt.Errorf("opening %s on %s: %w", direction, groupID, err)
			}
		}
	}
	for _, r := range have {
		if r.Direction == direction {
			continue
		}
		if err := rules.Delete(ctx, n.Client, r.ID).ExtractErr(); err != nil && !isGone(err) {
			return fmt.Errorf("removing a %s rule from the %s group: %w",
				r.Direction, direction, err)
		}
	}
	return nil
}

// EnsureDenyAll makes sure the anchor group exists and is empty.
//
// ⚠️ Emptying it is the work. Neutron adds two egress allow-all rules to every
// group it creates, so a group made to allow nothing arrives allowing all
// egress -- and a pod isolated in both directions would keep talking outward
// while appearing to be fully isolated.
func (n *Neutron) EnsureDenyAll(ctx context.Context) (string, error) {
	id, err := n.ensureGroup(ctx, DenyAll,
		"kubezun: allows nothing; carried so a port's group list is never empty")
	if err != nil {
		return "", err
	}
	have, err := n.rulesOf(ctx, id)
	if err != nil {
		return "", err
	}
	for _, r := range have {
		if err := rules.Delete(ctx, n.Client, r.ID).ExtractErr(); err != nil && !isGone(err) {
			return "", fmt.Errorf("emptying the deny-all group: %w", err)
		}
	}
	return id, nil
}

// findGroup returns a security group's id and description, or "" when the
// project has none by that name.
func (n *Neutron) findGroup(ctx context.Context, name string) (string, string, error) {
	var found, description string
	err := groups.List(n.Client, groups.ListOpts{Name: name}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := groups.ExtractGroups(page)
			if err != nil {
				return false, err
			}
			if len(list) > 0 {
				found, description = list[0].ID, list[0].Description
				return false, nil
			}
			return true, nil
		})
	if err != nil {
		return "", "", fmt.Errorf("looking for security group %s: %w", name, err)
	}
	return found, description, nil
}

func (n *Neutron) ensureGroup(ctx context.Context, name, description string) (string, error) {
	found, have, err := n.findGroup(ctx, name)
	if err != nil {
		return "", err
	}
	if found != "" {
		if have != description {
			// ⚠️ Rewritten when it differs, because the description is not
			// decoration: the sweep reads which policy a group belongs to out
			// of it, and a group left carrying an older wording is one the
			// sweep can never identify and so can never collect.
			if _, err := groups.Update(ctx, n.Client, found,
				groups.UpdateOpts{Description: &description}).Extract(); err != nil {
				return "", fmt.Errorf("correcting the description of %s: %w", name, err)
			}
		}
		return found, nil
	}
	g, err := groups.Create(ctx, n.Client, groups.CreateOpts{
		Name: name, Description: description + " [" + managedBy + "]",
	}).Extract()
	if err != nil {
		return "", fmt.Errorf("creating security group %s: %w", name, err)
	}
	return g.ID, nil
}

func (n *Neutron) rulesOf(ctx context.Context, groupID string) ([]rules.SecGroupRule, error) {
	var out []rules.SecGroupRule
	err := rules.List(n.Client, rules.ListOpts{SecGroupID: groupID}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := rules.ExtractRules(page)
			if err != nil {
				return false, err
			}
			out = append(out, list...)
			return true, nil
		})
	return out, err
}

// EnsureRuleSet makes a security group hold exactly these rules.
//
// The peers map turns a selector key into the address group id the rule refers
// to; every key in the set must be present, because a rule naming a group that
// does not exist is refused and leaves the set half written.
func (n *Neutron) EnsureRuleSet(ctx context.Context, name, policyRef string, set RuleSet, peers map[string]string) (string, error) {
	// ⚠️ The policy this belongs to goes in the description, because the name
	// is a hash and nothing can be read back out of it. Without it the sweep
	// cannot tell a group whose policy is gone from one belonging to a
	// namespace this process does not serve -- and it would have to choose
	// between leaking groups forever and deleting another process's.
	id, err := n.ensureGroup(ctx, name,
		"kubezun: what NetworkPolicy "+policyRef+" allows")
	if err != nil {
		return "", err
	}
	have, err := n.rulesOf(ctx, id)
	if err != nil {
		return "", err
	}

	want := make([]rules.CreateOpts, 0, len(set.Rules))
	for _, r := range set.Rules {
		opts := rules.CreateOpts{
			SecGroupID:   id,
			Direction:    rules.RuleDirection(r.Direction),
			EtherType:    rules.RuleEtherType(r.EtherType),
			Protocol:     rules.RuleProtocol(r.Protocol),
			PortRangeMin: int(r.PortMin),
			PortRangeMax: int(r.PortMax),
		}
		switch {
		case r.RemoteCIDR != "":
			opts.RemoteIPPrefix = r.RemoteCIDR
		case r.RemoteAddressGroup != "":
			gid, ok := peers[r.RemoteAddressGroup]
			if !ok || gid == "" {
				return "", fmt.Errorf(
					"the peer set %q has no address group yet", r.RemoteAddressGroup)
			}
			opts.RemoteAddressGroupID = gid
		}
		want = append(want, opts)
	}

	for _, w := range want {
		if hasRule(have, w) {
			continue
		}
		if _, err := rules.Create(ctx, n.Client, w).Extract(); err != nil && !isConflict(err) {
			return "", fmt.Errorf("writing a rule into %s: %w", name, err)
		}
	}
	// ⚠️ And remove what is no longer wanted, including the two egress
	// allow-all rules Neutron adds to every group it creates. A rule set that
	// only ever grows is a rule set that keeps allowing what a tenant has
	// already taken out of its policy.
	for _, h := range have {
		if wantsRule(want, h) {
			continue
		}
		if err := rules.Delete(ctx, n.Client, h.ID).ExtractErr(); err != nil && !isGone(err) {
			return "", fmt.Errorf("removing a stale rule from %s: %w", name, err)
		}
	}
	return id, nil
}

// policyOf reads back which policy a group was made for, or "" if it was not
// made by this.
func policyOf(description string) string {
	const prefix = "kubezun: what NetworkPolicy "
	const suffix = " allows"
	if !strings.HasPrefix(description, prefix) || !strings.HasSuffix(description, suffix) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(description, prefix), suffix)
}

// Sweep removes the groups no live policy needs any more.
//
// ⚠️ Deliberately not driven by policy deletion. Deleting a security group
// object makes ovn-northd rebuild every port group in the cloud -- other
// tenants', nova's, Octavia's -- so the cost of removing one is paid by
// everybody. Doing it on a slow timer, in a batch, spends that once for
// whatever has accumulated instead of once per policy a tenant edits.
//
// live is the set of policy references ("namespace/name") that still exist.
// A group is removed only when this process serves its namespace and the
// policy is gone: a group whose policy exists but whose pods have all left is
// kept, because the pods will come back and rebuilding it costs more than
// leaving it.
func (n *Neutron) Sweep(ctx context.Context, live map[string]bool, serves func(string) bool) (removed int, err error) {
	var all []groups.SecGroup
	err = groups.List(n.Client, groups.ListOpts{}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			got, err := groups.ExtractGroups(page)
			if err != nil {
				return false, err
			}
			all = append(all, got...)
			return true, nil
		})
	if err != nil {
		return 0, fmt.Errorf("listing security groups: %w", err)
	}

	// ⚠️ Decide what goes before working out what is still referenced. A peer
	// set pointed at only by a group that is about to be deleted is not
	// referenced by anything, and computing that from the list as read leaves
	// it behind for a whole sweep interval -- correct in the end, and a
	// needlessly long end.
	doomed := map[string]struct{}{}
	for _, g := range all {
		if !strings.HasPrefix(g.Name, "knp-policy-") {
			continue
		}
		ref := policyOf(g.Description)
		if ref == "" {
			// Not identifiable, so not ours to judge: it may belong to a
			// namespace this process does not serve.
			continue
		}
		namespace, _, ok := strings.Cut(ref, "/")
		if !ok || !serves(namespace) || live[ref] {
			continue
		}
		doomed[g.ID] = struct{}{}
	}

	// Which peer sets the survivors still point at -- including groups
	// belonging to namespaces this process does not serve, whose rules are
	// just as real.
	referenced := map[string]struct{}{}
	for _, g := range all {
		if !strings.HasPrefix(g.Name, "knp-policy-") {
			continue
		}
		if _, going := doomed[g.ID]; going {
			continue
		}
		for _, r := range g.Rules {
			if r.RemoteAddressGroupID != "" {
				referenced[r.RemoteAddressGroupID] = struct{}{}
			}
		}
	}

	for _, g := range all {
		if _, going := doomed[g.ID]; !going {
			continue
		}
		if err := groups.Delete(ctx, n.Client, g.ID).ExtractErr(); err != nil {
			if isConflict(err) || isGone(err) {
				// Still on a port somewhere: a pod this process has not
				// finished reconciling. It will come round again.
				continue
			}
			return removed, fmt.Errorf("removing %s: %w", g.Name, err)
		}
		removed++
	}

	// Peer sets are named by prefix and referenced by id, so an unreferenced
	// one is unreferenced for everybody.
	var peers []addressgroups.AddressGroup
	err = addressgroups.List(n.Client, addressgroups.ListOpts{}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			got, err := addressgroups.ExtractGroups(page)
			if err != nil {
				return false, err
			}
			peers = append(peers, got...)
			return true, nil
		})
	if err != nil {
		return removed, fmt.Errorf("listing address groups: %w", err)
	}
	for _, p := range peers {
		if !strings.HasPrefix(p.Name, "knp-peers-") {
			continue
		}
		if _, still := referenced[p.ID]; still {
			continue
		}
		if err := addressgroups.Delete(ctx, n.Client, p.ID).ExtractErr(); err != nil {
			if isConflict(err) || isGone(err) {
				continue
			}
			return removed, fmt.Errorf("removing peer set %s: %w", p.Name, err)
		}
		removed++
	}
	return removed, nil
}

// EnsureAddressGroup makes sure a peer set exists under a stable name.
func (n *Neutron) EnsureAddressGroup(ctx context.Context, name string) (string, error) {
	var found string
	err := addressgroups.List(n.Client, addressgroups.ListOpts{Name: name}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := addressgroups.ExtractGroups(page)
			if err != nil {
				return false, err
			}
			if len(list) > 0 {
				found = list[0].ID
				return false, nil
			}
			return true, nil
		})
	if err != nil {
		return "", fmt.Errorf("looking for address group %s: %w", name, err)
	}
	if found != "" {
		return found, nil
	}
	// ⚠️ Posted directly rather than through addressgroups.CreateOpts, whose
	// Addresses field is marked required by the client while the service
	// accepts an empty group and answers 201 (measured). An empty peer set is
	// an ordinary state -- a selector that matches no pod yet -- so inventing
	// a placeholder address to satisfy a client-side rule would put an address
	// in a group that nothing asked to be there.
	var created struct {
		Group struct {
			ID string `json:"id"`
		} `json:"address_group"`
	}
	body := map[string]any{"address_group": map[string]any{
		"name":        name,
		"description": managedBy,
		"addresses":   []string{},
	}}
	if _, err := n.Client.Post(ctx, n.Client.ServiceURL("address-groups"),
		body, &created, &gophercloud.RequestOpts{
			OkCodes: []int{200, 201}}); err != nil {
		return "", fmt.Errorf("creating address group %s: %w", name, err)
	}
	if created.Group.ID == "" {
		return "", fmt.Errorf("creating address group %s: no id was returned", name)
	}
	return created.Group.ID, nil
}

// SyncAddresses makes an address group hold exactly these addresses.
//
// ⚠️ Read, diff, then write. The add and remove calls are not idempotent --
// adding an address that is already there answers 400 AddressesAlreadyExist,
// and removing one that is not there answers 400 AddressesNotFound (measured,
// not assumed). A reconciler that retries must therefore never send the whole
// desired set and hope; it sends the difference, and treats those two answers
// as agreement rather than failure when a concurrent write got there first.
func (n *Neutron) SyncAddresses(ctx context.Context, groupID string, want []string) error {
	current, err := addressgroups.Get(ctx, n.Client, groupID).Extract()
	if err != nil {
		return fmt.Errorf("reading address group %s: %w", groupID, err)
	}
	add, remove := diffAddresses(current.Addresses, want)
	if len(add) > 0 {
		if _, err := addressgroups.AddAddresses(ctx, n.Client, groupID,
			addressgroups.UpdateAddressesOpts{Addresses: add}).Extract(); err != nil {
			if !isConflict(err) {
				return fmt.Errorf("adding %d addresses to %s: %w", len(add), groupID, err)
			}
		}
	}
	if len(remove) > 0 {
		if _, err := addressgroups.RemoveAddresses(ctx, n.Client, groupID,
			addressgroups.UpdateAddressesOpts{Addresses: remove}).Extract(); err != nil {
			if !isConflict(err) {
				return fmt.Errorf("removing %d addresses from %s: %w", len(remove), groupID, err)
			}
		}
	}
	return nil
}

// diffAddresses says what to add and what to remove to get from have to want.
//
// Addresses are compared in the form Neutron stores them, which is a CIDR: a
// bare address handed in as "10.0.0.5" comes back as "10.0.0.5/32", and
// comparing the two forms directly would re-add every address on every pass.
func diffAddresses(have, want []string) (add, remove []string) {
	h := make(map[string]struct{}, len(have))
	for _, a := range have {
		h[normalizeCIDR(a)] = struct{}{}
	}
	w := make(map[string]struct{}, len(want))
	for _, a := range want {
		w[normalizeCIDR(a)] = struct{}{}
	}
	for a := range w {
		if _, ok := h[a]; !ok {
			add = append(add, a)
		}
	}
	for a := range h {
		if _, ok := w[a]; !ok {
			remove = append(remove, a)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	return add, remove
}

func normalizeCIDR(a string) string {
	a = strings.TrimSpace(a)
	if a == "" || strings.Contains(a, "/") {
		return a
	}
	if strings.Contains(a, ":") {
		return a + "/128"
	}
	return a + "/32"
}

// isConflict reports whether the service is saying "already so", which for an
// operation whose goal is a state rather than a change is agreement.
func isConflict(err error) bool {
	var conflict gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &conflict) {
		if conflict.Actual == 409 {
			return true
		}
		if conflict.Actual == 400 {
			body := string(conflict.Body)
			return strings.Contains(body, "AddressesAlreadyExist") ||
				strings.Contains(body, "AddressesNotFound") ||
				strings.Contains(body, "already exists")
		}
	}
	return false
}

// isGone reports whether the thing we were removing is already absent, which
// is the outcome the caller wanted.
func isGone(err error) bool {
	var code gophercloud.ErrUnexpectedResponseCode
	return errors.As(err, &code) && code.Actual == 404
}

// wantsRule reports whether an existing rule is one of the wanted ones.
func wantsRule(want []rules.CreateOpts, have rules.SecGroupRule) bool {
	for _, w := range want {
		if hasRule([]rules.SecGroupRule{have}, w) {
			return true
		}
	}
	return false
}

func hasRule(have []rules.SecGroupRule, want rules.CreateOpts) bool {
	for _, r := range have {
		if r.Direction == string(want.Direction) &&
			r.EtherType == string(want.EtherType) &&
			r.Protocol == string(want.Protocol) &&
			r.PortRangeMin == want.PortRangeMin &&
			r.PortRangeMax == want.PortRangeMax &&
			r.RemoteIPPrefix == want.RemoteIPPrefix &&
			r.RemoteGroupID == want.RemoteGroupID &&
			r.RemoteAddressGroupID == want.RemoteAddressGroupID {
			return true
		}
	}
	return false
}
