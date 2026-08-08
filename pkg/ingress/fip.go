package ingress

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	networkingv1 "k8s.io/api/networking/v1"
)

// internal reports whether the Ingress stays VIP-only — the DEFAULT: exposing
// a tenant HTTPS endpoint to the world is an explicit ask, not a side effect,
// and a public address is billed.
func internal(ing *networkingv1.Ingress) bool {
	return ing.Annotations[InternalAnnotation] != "false"
}

// Ownership of a floating IP is carried in its DESCRIPTION, not in an
// annotation on the Ingress. The implementation this is adapted from persisted
// the FIP id on the Ingress and read it back at teardown — which needs the
// Ingress object, and this package's teardown deliberately works without one
// (name-derived, sweep-recoverable, like pkg/service). The description is on
// the OpenStack side of the fence, so the sweep can make the same
// delete-or-detach decision the live path makes:
//
//	<lbname>        created by this controller — delete on teardown
//	<lbname>_keep   created by this controller, keep-floatingip=true — detach
//	anything else   the tenant's own — detach, never delete
func fipDescription(lbname string, keep bool) string {
	if keep {
		return lbname + "_keep"
	}
	return lbname
}

// ensureFIP realises the Ingress's exposure request and returns the externally
// reachable address ("" when internal).
func (r *Reconciler) ensureFIP(ctx context.Context, ing *networkingv1.Ingress, lbname, vipPortID string) (string, error) {
	if internal(ing) {
		// Asked for one before and does not now: give it back rather than
		// leave it allocated and billed.
		return "", r.releaseFIP(ctx, lbname, vipPortID)
	}

	keep := ing.Annotations[KeepFloatingIPAnnotation] == "true"

	// Tenant-specified address: bind their pre-existing floating IP, never
	// create or delete one.
	if addr := ing.Annotations[FloatingIPAnnotation]; addr != "" {
		fip, err := fipByAddress(ctx, r.Neutron, addr)
		if err != nil {
			return "", err
		}
		if fip.PortID != vipPortID {
			if _, err := floatingips.Update(ctx, r.Neutron, fip.ID, floatingips.UpdateOpts{PortID: &vipPortID}).Extract(); err != nil {
				return "", fmt.Errorf("associating floating IP %s with the listener: %w", addr, err)
			}
		}
		return fip.FloatingIP, nil
	}

	// Ours: find by the port it is bound to, else allocate.
	fip, err := fipForPort(ctx, r.Neutron, vipPortID)
	if err != nil {
		return "", err
	}
	if fip == nil {
		if r.FloatingNetworkID == "" {
			return "", fmt.Errorf("the Ingress asks for external exposure (%s=false) but this deployment has no floating network", InternalAnnotation)
		}
		fip, err = floatingips.Create(ctx, r.Neutron, floatingips.CreateOpts{
			FloatingNetworkID: r.FloatingNetworkID,
			PortID:            vipPortID,
			Description:       fipDescription(lbname, keep),
		}).Extract()
		if err != nil {
			return "", fmt.Errorf("allocating a floating IP on network %s: %w", r.FloatingNetworkID, err)
		}
		return fip.FloatingIP, nil
	}
	// Keep the ownership marker in step with the annotation, so a teardown
	// that never sees the Ingress still honours keep-floatingip.
	if want := fipDescription(lbname, keep); fip.Description != want && ownedBy(fip.Description, lbname) {
		if _, err := floatingips.Update(ctx, r.Neutron, fip.ID, floatingips.UpdateOpts{Description: &want}).Extract(); err != nil {
			return "", fmt.Errorf("updating floating IP ownership marker: %w", err)
		}
	}
	return fip.FloatingIP, nil
}

// releaseFIP undoes ensureFIP: a floating IP this controller created is
// deleted (detached instead when it was marked keep); anything else bound to
// the port — the tenant's own — is only detached.
func (r *Reconciler) releaseFIP(ctx context.Context, lbname, vipPortID string) error {
	if vipPortID == "" {
		return nil
	}
	fip, err := fipForPort(ctx, r.Neutron, vipPortID)
	if err != nil || fip == nil {
		return err
	}
	if fip.Description == fipDescription(lbname, false) {
		if err := floatingips.Delete(ctx, r.Neutron, fip.ID).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, 404) {
			return fmt.Errorf("deleting floating IP %s: %w", fip.FloatingIP, err)
		}
		return nil
	}
	log.G(ctx).WithField("fip", fip.FloatingIP).Info("detaching a floating IP that is not ours to delete")
	_, err = floatingips.Update(ctx, r.Neutron, fip.ID, floatingips.UpdateOpts{PortID: nil}).Extract()
	if err != nil && !gophercloud.ResponseCodeIs(err, 404) {
		return err
	}
	return nil
}

// ownedBy reports whether a description marks the floating IP as this
// load balancer's.
func ownedBy(description, lbname string) bool {
	return description == lbname || description == lbname+"_keep"
}

// fipForPort finds the floating IP bound to a port, or nil.
func fipForPort(ctx context.Context, neutron *gophercloud.ServiceClient, portID string) (*floatingips.FloatingIP, error) {
	pages, err := floatingips.List(neutron, floatingips.ListOpts{PortID: portID}).AllPages(ctx)
	if err != nil {
		return nil, err
	}
	fips, err := floatingips.ExtractFloatingIPs(pages)
	if err != nil {
		return nil, err
	}
	if len(fips) == 0 {
		return nil, nil
	}
	return &fips[0], nil
}

// fipByAddress resolves one floating IP by its address.
func fipByAddress(ctx context.Context, neutron *gophercloud.ServiceClient, addr string) (*floatingips.FloatingIP, error) {
	pages, err := floatingips.List(neutron, floatingips.ListOpts{FloatingIP: addr}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing floating IP %s: %w", addr, err)
	}
	fips, err := floatingips.ExtractFloatingIPs(pages)
	if err != nil {
		return nil, err
	}
	if len(fips) == 0 {
		return nil, fmt.Errorf("floating IP %s does not exist in this project (the %s annotation must name an existing one)", addr, FloatingIPAnnotation)
	}
	return &fips[0], nil
}
