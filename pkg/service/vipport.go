package service

import (
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
)

// A load balancer's address is held by a port this package creates, and the
// port id is handed to Octavia rather than letting it allocate one.
//
// ⚠️ Otherwise the address is not reserved. The OVN provider's own address port
// stops existing shortly after the load balancer is created — the load balancer
// keeps working, because the provider carries the address in the data plane —
// and nothing then holds the address in Neutron, so the next allocation on that
// subnet can hand out the same one. Measured: kube-dns and probesvc, two ACTIVE
// load balancers, both on 192.168.200.36, which makes the traffic for one
// tenant's Service arrive at another's pool.
//
// A port created here is owned here: Octavia does not delete a VIP port it was
// given rather than made, so tearing a load balancer down has to remove it, and
// the sweep has to reclaim the ones left behind.

// vipPortName is the port holding one Service's address. Derived from the load
// balancer's name so the two can always be matched up, including by a sweep
// that has only the port to go on.
func vipPortName(lbName string) string { return lbName + "_vip" }

// ensureVIPPort returns the id of the port holding this Service's address,
// creating it if there is none.
//
// Reused rather than recreated when it already exists: the address is the
// Service's identity to everything that has resolved it, and this is the object
// that keeps it.
func ensureVIPPort(ctx context.Context, neutron *gophercloud.ServiceClient, networkID, subnetID, lbName string) (string, error) {
	if neutron == nil {
		return "", fmt.Errorf("no Neutron client: a Service's address cannot be reserved")
	}
	name := vipPortName(lbName)

	existing, err := findPortByName(ctx, neutron, name)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil
	}

	port, err := ports.Create(ctx, neutron, ports.CreateOpts{
		Name:      name,
		NetworkID: networkID,
		FixedIPs:  []ports.IP{{SubnetID: subnetID}},
		// The load balancer answers on this address without the port being
		// bound to anything, and a port with security groups applied would
		// have its traffic filtered by rules meant for a machine.
		Description: "kubezun: holds a Service's load balancer address",
	}).Extract()
	if err != nil {
		return "", fmt.Errorf("reserving an address for %s on subnet %s: %w",
			lbName, subnetID, err)
	}
	return port.ID, nil
}

// findPortByName returns the id of the port with this exact name, or empty.
func findPortByName(ctx context.Context, neutron *gophercloud.ServiceClient, name string) (string, error) {
	pages, err := ports.List(neutron, ports.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("looking for port %q: %w", name, err)
	}
	all, err := ports.ExtractPorts(pages)
	if err != nil {
		return "", err
	}
	switch len(all) {
	case 0:
		return "", nil
	case 1:
		return all[0].ID, nil
	default:
		// Two ports under one name means something else is creating them, and
		// picking one would leave a second address allocated and unreachable.
		return "", fmt.Errorf("%d ports are named %q", len(all), name)
	}
}

// releaseVIPPort removes the port holding a load balancer's address.
//
// Called after the load balancer is gone, never before: while it exists the
// address is in use, and Octavia will refuse to delete a port it is using in
// any case.
func releaseVIPPort(ctx context.Context, neutron *gophercloud.ServiceClient, lbName string) error {
	if neutron == nil {
		return nil
	}
	id, err := findPortByName(ctx, neutron, vipPortName(lbName))
	if err != nil || id == "" {
		return err
	}
	if err := ports.Delete(ctx, neutron, id).ExtractErr(); err != nil {
		if gophercloud.ResponseCodeIs(err, 404) {
			return nil
		}
		return fmt.Errorf("releasing the address port of %s: %w", lbName, err)
	}
	return nil
}
