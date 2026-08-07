package service

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	corev1 "k8s.io/api/core/v1"
)

const (
	// InternalAnnotation keeps a Service off the public network. The name is
	// the one cloud-provider-openstack uses, so a chart written for an
	// OpenStack cluster behaves the same here.
	InternalAnnotation = "service.beta.kubernetes.io/openstack-internal-load-balancer"

	// FloatingNetworkAnnotation picks which external network the address comes
	// from, for a deployment with more than one.
	FloatingNetworkAnnotation = "loadbalancer.openstack.org/floating-network-id"
)

// wantsPublicAddress reports whether this Service should be reachable from
// outside the tenant's network.
//
// Only a LoadBalancer Service can be: every Service gets a load balancer here,
// so type is the one thing a tenant writes that says "and reachable from
// outside".
//
// The default is private. A public address costs the platform real money and a
// tenant who wanted one and got a private address opens a ticket, where a
// tenant who did not want one and got a public address has an incident. The
// platform can flip the default; cloud-provider-openstack defaults the other
// way because it usually serves one organisation's own cluster.
func wantsPublicAddress(svc *corev1.Service, defaultPublic bool) bool {
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return false
	}
	switch svc.Annotations[InternalAnnotation] {
	case "true":
		return false
	case "false":
		return true
	}
	return defaultPublic
}

// ensureFloatingIP gives the load balancer's address a public one, and returns
// it.
//
// Found by the port it is attached to rather than by anything recorded on the
// Service: an address that a lost annotation made invisible would stay
// allocated and charged for while a second one was created beside it.
func ensureFloatingIP(
	ctx context.Context,
	neutron *gophercloud.ServiceClient,
	vipPortID, floatingNetworkID string,
) (string, error) {

	existing, err := floatingIPForPort(ctx, neutron, vipPortID)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return existing.FloatingIP, nil
	}
	if floatingNetworkID == "" {
		return "", fmt.Errorf(
			"no external network is configured, so a public address cannot be given; " +
				"set one on the node or ask for an internal load balancer")
	}

	fip, err := floatingips.Create(ctx, neutron, floatingips.CreateOpts{
		FloatingNetworkID: floatingNetworkID,
		PortID:            vipPortID,
	}).Extract()
	if err != nil {
		return "", fmt.Errorf("allocating a public address on network %s: %w",
			floatingNetworkID, err)
	}
	return fip.FloatingIP, nil
}

// releaseFloatingIP gives a public address back.
//
// Called before the load balancer is deleted, while the address can still be
// found by the port it is on. Deleting the load balancer first takes the port
// with it, and Neutron then leaves the address allocated to the project but
// attached to nothing — still billed, and no longer connected to anything that
// would lead someone back to it.
func releaseFloatingIP(ctx context.Context, neutron *gophercloud.ServiceClient, vipPortID string) error {
	fip, err := floatingIPForPort(ctx, neutron, vipPortID)
	if err != nil {
		return err
	}
	if fip == nil {
		return nil
	}
	if err := floatingips.Delete(ctx, neutron, fip.ID).ExtractErr(); err != nil &&
		!gophercloud.ResponseCodeIs(err, 404) {
		return fmt.Errorf("releasing public address %s: %w", fip.FloatingIP, err)
	}
	return nil
}

func floatingIPForPort(ctx context.Context, neutron *gophercloud.ServiceClient, portID string) (*floatingips.FloatingIP, error) {
	if portID == "" {
		return nil, nil
	}
	pages, err := floatingips.List(neutron, floatingips.ListOpts{PortID: portID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("looking for a public address on port %s: %w", portID, err)
	}
	all, err := floatingips.ExtractFloatingIPs(pages)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	return &all[0], nil
}
