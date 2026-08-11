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
// families. This is what "no policy selects this pod" means in Kubernetes, and
// Neutron's baseline is the opposite -- a port with security groups is denied
// what no rule allows -- so the permissive case has to be stated.
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
			// the read and the write. That is the same outcome we wanted.
			if !isConflict(err) {
				return fmt.Errorf("opening %s on %s: %w", direction, groupID, err)
			}
		}
	}
	return nil
}

func (n *Neutron) ensureGroup(ctx context.Context, name, description string) (string, error) {
	var found string
	err := groups.List(n.Client, groups.ListOpts{Name: name}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			list, err := groups.ExtractGroups(page)
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
		return "", fmt.Errorf("looking for security group %s: %w", name, err)
	}
	if found != "" {
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
